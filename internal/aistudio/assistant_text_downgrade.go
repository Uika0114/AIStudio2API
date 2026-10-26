package aistudio

import "fmt"

// 本文件实现 Assistant（CodeAssistantOffline）文本通道的请求预处理：把客户端
// 工具契约降级成上游适配一致的纯文本请求。
//
// 方向依据：docs/decisions/assistant文本接入沿用上游工具降级.md（accepted）。
// 上游固定基准：Mag1cFall/AIStudio2API v0.2.13 @
// a784c5193b3ba5c4894d536b2f4cd73e87f99475
//
//   - internal/aistudio/tool_validation.go:10-58  supportedTools
//   - internal/aistudio/tool_validation.go:61-81  transcribeFunctionParts
//   - internal/aistudio/tool_validation.go:84-101 toolPartTranscript / transcriptJSON
//   - internal/aistudio/generate.go:679-688        过滤与历史转换的接线
//
// 与上游 supportedTools 的差异（本模块的规范化选择，不是上游行为的复制）：
// 上游在 ToolConfig.Mode == "none" 时直接原样返回 tools；但 Assistant 的
// CodeAssistantOffline wire 没有任何已验证的工具声明/选择字段
// （docs/assistant-tool-wire-evidence.md），因此本模块无论 mode 是 none、
// auto 还是 required，都把 Tools 整体置零，声明、allowed、parallel 一并清除，
// 既不透传声明，也不把 none 误判成“原生工具已被支持”。这样 Assistant wire
// 上不会出现工具 envelope，客户端也不会据此以为工具闭环可用。
//
// 本模块不做任何 RPC、Applet 创建或资源访问，也不伪造原生工具调用：函数历史
// 只被改写成 JSON 文本。合法显式输出预算只做兼容清理，不兑现该上限；
// 媒体、sampling、安全及其余限制仍由既有 validator/strict entry 处理。
//
// 已知限制（本次候选明确不扩大能力）：ExecutableCode / CodeExecutionResult
// 历史不在此转录。上游 transcribeFunctionParts 只处理 function part
// （tool_validation.go:70），虽然 toolPartTranscript 能序列化 code
// （tool_validation.go:91-94），本候选保留 code Part 原样，交由后续 validator
// 拒绝，不在文本降级里把 code 历史变成可发送的正文。

// assistantKnownGoogleTools 是上游 supportedTools 承认的 Google 内置工具名
// （tool_validation.go:32-41 的 switch）。Assistant 没有任何 Google 工具能力，
// 因此这些已知名字全部被过滤；只有未知名字按上游规则报错。
var assistantKnownGoogleTools = map[string]bool{
	"code_execution": true,
	"google_search":  true,
	"image_search":   true,
	"google_maps":    true,
	"url_context":    true,
}

// prepareAssistantTextRequest 把 Assistant 通道的客户端工具契约降级为上游适配
// 一致的纯文本请求，并返回独立的请求副本。它只对 Selection.Channel ==
// ChannelAssistant 生效；其它通道（Playground / Build / 自动路由）原样返回，
// 绝不削弱 PG/Build 既有的工具与抗截断合同。
//
// 行为：
//   - Selection.AntiTruncation 在 Assistant 上显式拒绝，不降级为普通文本请求；
//   - 未知 Google tool 按上游规则报错，已知但不支持的 Google tool 被过滤；
//   - 函数调用/结果历史改写成上游一致的 JSON 文本，缺 name 的结果先按调用 ID、
//     再按唯一待返回调用补齐名称，无法关联时报错；
//   - 工具声明、tool choice、allowed function names、parallel calls 全部清除
//     （Tools 整体置零），与 mode 无关；
//   - 正整数 MaxOutputTokens 在副本上置 nil；非正值拒绝，未指定不补默认；
//   - 不改动调用方传入的 request 及其 Contents / Parts / RawMessage。
//
// 幂等：对同一请求重复调用得到相等结果。
func prepareAssistantTextRequest(request GenerateRequest) (GenerateRequest, error) {
	if request.Selection.Channel != ChannelAssistant {
		// 非 Assistant 原样返回，PG/Build 的工具与抗截断路径完全不受影响。
		return request, nil
	}
	if request.Selection.AntiTruncation {
		return GenerateRequest{}, assistantToolMappingError("the anti-truncation transport tool is refused on the assistant text route; it is never downgraded to a plain text request")
	}
	if budget := request.Config.MaxOutputTokens; budget != nil {
		if *budget <= 0 {
			return GenerateRequest{}, fmt.Errorf("%w: assistant max output token budget must be positive when specified", ErrInvalidArgument)
		}
		// The Config is a value copy: never mutate the caller-owned pointer target.
		// CodeAssistantOffline has no supported output-budget control.
		request.Config.MaxOutputTokens = nil
	}
	for _, tool := range request.Tools.Google {
		if !assistantKnownGoogleTools[tool] {
			// 上游 supportedTools 对未知 Google tool 直接报错，而不是静默过滤。
			return GenerateRequest{}, fmt.Errorf("%w: 未知 Google tool %q", ErrInvalidArgument, tool)
		}
	}
	contents, err := transcribeAssistantFunctionHistory(request.Contents)
	if err != nil {
		return GenerateRequest{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	request.Contents = contents
	// Assistant 的 CodeAssistantOffline wire 没有已验证的工具声明/选择字段，
	// 因此无论客户端 mode 是什么都整体置零。这里刻意不调用 supportedTools：
	// 它在 mode=none 时会原样透传声明，而 Assistant 连透传都没有已验证的承载字段。
	request.Tools = Tools{}
	return request, nil
}

// transcribeAssistantFunctionHistory 把函数调用/结果历史改写为上游一致的 JSON
// 文本，返回独立副本，绝不改动传入的 contents。
//
// 它复用共享的 linkFunctionResults（request.go:119）补齐缺失名称：先按调用 ID，
// 再按唯一待返回调用关联；无法关联时报错。转录复用 toolPartTranscript
// （tool_validation.go:84），因此文本格式与上游一致。
//
// 与上游 transcribeFunctionParts（tool_validation.go:61-81）的差异：上游在循环里
// 读 linked.Parts 却写回 content.Parts；当 linkFunctionResults 因补齐名称克隆了
// Parts 时，写回的是旧切片，转录与名称补齐之间靠别名巧合保持一致。本实现把
// linked.Parts 作为唯一工作切片并写回 transcribed[index]，不复制该巧合。
func transcribeAssistantFunctionHistory(contents []Content) ([]Content, error) {
	if len(contents) == 0 {
		// 保持 nil / 空切片的原样身份，避免把 nil contents 变成空数组。
		return contents, nil
	}
	transcribed := cloneContentsForFileCopies(contents)
	var pendingCalls []FunctionCall
	for contentIndex := range transcribed {
		linked, err := linkFunctionResults(transcribed[contentIndex], &pendingCalls)
		if err != nil {
			return nil, fmt.Errorf("contents[%d]: %w", contentIndex, err)
		}
		for partIndex := range linked.Parts {
			part := linked.Parts[partIndex]
			if part.FunctionCall == nil && part.FunctionResult == nil {
				continue
			}
			// Transcription must not erase a different unsupported content variant.
			if part.FunctionCall != nil && part.FunctionResult != nil || part.Text != "" || part.InlineData != nil || part.ExternalMedia != nil || part.File != nil || part.ExecutableCode != nil || part.CodeExecutionResult != nil {
				return nil, fmt.Errorf("contents[%d].parts[%d]: assistant function history part has multiple content variants", contentIndex, partIndex)
			}
			transcript, err := toolPartTranscript(part)
			if err != nil {
				return nil, fmt.Errorf("contents[%d].parts[%d]: %w", contentIndex, partIndex, err)
			}
			// 只保留转录文本：函数签名与不透明 thought signature 不再随文本带出。
			linked.Parts[partIndex] = Part{Text: transcript}
		}
		transcribed[contentIndex] = linked
	}
	return transcribed, nil
}
