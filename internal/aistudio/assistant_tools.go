package aistudio

import (
	"encoding/json"
	"fmt"
)

// 本文件集中记录并执行 Assistant（CodeAssistantOffline）的客户端工具边界。
// 它只承认有公开来源的字段；没有来源的字段不写入、不猜测，也不把工具声明
// 或历史静默删除后当作成功。
//
// 固定证据（2026-10-19 核查，完整来源与偏移见
// docs/assistant-tool-wire-evidence.md）：
//
//  1. 上游 Mag1cFall/AIStudio2API v0.2.13 @
//     a784c5193b3ba5c4894d536b2f4cd73e87f99475
//     internal/aistudio/assistant.go:94-119 的 EncodeAssistantRequest 与
//     docs/build.md:558-584 的请求映射表只给出 contents(field 1)、proof(2)、
//     mode(6)、model(8)、system(9)、applet(12)、framework(14)、16、thinking(19)
//     与请求 ID(29)。没有任何字段被记录为函数声明或工具选择。
//  2. docs/build.md:566 明确 field 1 的 contents 与 Playground GenerateContent
//     使用同一套编码，因此共享 Content Part 的 JSON 索引 10（functionCall）
//     与 11（functionResponse）可以作为历史运输。
//  3. AI Studio 前端基础 bundle 只定义 RPC 与请求类的 apiKey(field 13)/field 27
//     访问器，不构造 CodeAssistantOffline 请求；它既没有给出函数声明/工具选择
//     字段，也不能证明这些字段一定不存在。上游把历史转文本是适配选择，不是
//     “底层不支持工具”的证据。
//
// 因此当前可声称的客户端工具能力只有“历史可编码”：没有声明/选择 envelope
// 时，历史本身不能授权客户端工具，不能满足 required，也不能完成工具闭环。

const (
	// assistantContentsFieldNumber 是 CodeAssistantOffline 的对话轮字段。
	// 来源：上游 a784c519 internal/aistudio/assistant.go:107 与 docs/build.md:566。
	assistantContentsFieldNumber = 1
	// assistantFunctionCallPartIndex 是共享 Content Part 中 functionCall 的
	// JSON 索引（protobuf field 11）。来源：docs/protocol.md:595-596；
	// 本地 request.go 的 encodePart 使用同一索引。
	assistantFunctionCallPartIndex = 10
	// assistantFunctionResponsePartIndex 是共享 Content Part 中
	// functionResponse 的 JSON 索引（protobuf field 12）。
	assistantFunctionResponsePartIndex = 11
)

// assistantClientToolCapabilities 是 assistant 路线当前可以从静态证据声称的
// 客户端工具能力。零值之外只有 FunctionHistory 为 true：共享 Content 编码能
// 承载函数调用/结果历史；声明、选择、并行调用、Google 工具、抗截断运输工具
// 以及响应中的函数调用投影都没有验证过的映射。
type assistantClientToolCapabilities struct {
	Declarations          bool
	ToolChoice            bool
	ParallelCalls         bool
	FunctionHistory       bool
	ResponseFunctionCalls bool
	TransportTool         bool
}

// assistantClientToolCapabilitiesForWire 返回当前 wire 证据支持的能力集合。
// 任何一项要从 false 变成 true，必须先补上可核查的来源，而不是打开开关。
func assistantClientToolCapabilitiesForWire() assistantClientToolCapabilities {
	return assistantClientToolCapabilities{FunctionHistory: true}
}

// clientToolLoop reports whether the verified capabilities can complete a real
// client tool loop (declare -> call -> return result). History alone cannot.
func (c assistantClientToolCapabilities) clientToolLoop() bool {
	return c.Declarations && c.ToolChoice && c.FunctionHistory && c.ResponseFunctionCalls
}

// assistantToolMappingError 统一“实现缺口”错误：明确指出缺少映射，而不是
// 暗示上游不支持。调用方用 errors.Is(err, ErrInvalidArgument) 判定。
func assistantToolMappingError(detail string) error {
	return fmt.Errorf("%w: assistant tool protocol mapping not implemented: %s", ErrInvalidArgument, detail)
}

// assistantClientToolRefusal validates the normalized text-only wire shape.
// Public Assistant boundaries first apply prepareAssistantTextRequest: client
// declarations/choice are filtered and function history transcribed by policy.
// Any native leftovers (or anti/code history) remain invalid; this guard must
// not be used as the public user-facing tool downgrade itself.
func assistantClientToolRefusal(request GenerateRequest) error {
	if request.Selection.AntiTruncation {
		return assistantToolMappingError("the anti-truncation transport tool has no verified CodeAssistantOffline declaration/choice field")
	}
	tools := request.Tools
	if len(tools.Functions) > 0 {
		return assistantToolMappingError(fmt.Sprintf("%d client function declaration(s) have no verified CodeAssistantOffline field", len(tools.Functions)))
	}
	if len(tools.Google) > 0 || tools.GoogleSearch != nil {
		return assistantToolMappingError("Google tools have no verified CodeAssistantOffline field")
	}
	if tools.ToolConfig.Mode != "" && tools.ToolConfig.Mode != "none" {
		return assistantToolMappingError(fmt.Sprintf("tool choice %q has no verified CodeAssistantOffline field", tools.ToolConfig.Mode))
	}
	if len(tools.ToolConfig.AllowedFunctionNames) > 0 {
		return assistantToolMappingError("allowed function names have no verified CodeAssistantOffline field")
	}
	if tools.ToolConfig.ParallelCalls != nil {
		return assistantToolMappingError("parallel tool calls have no verified CodeAssistantOffline field")
	}
	for contentIndex, content := range request.Contents {
		for partIndex, part := range content.Parts {
			switch {
			case part.FunctionCall != nil || part.FunctionResult != nil:
				return assistantToolMappingError(fmt.Sprintf("contents[%d].parts[%d] carries native function history: the shared Content Part is encodable but no verified declaration/choice envelope authorizes client tools", contentIndex, partIndex))
			case part.ExecutableCode != nil || part.CodeExecutionResult != nil:
				return assistantToolMappingError(fmt.Sprintf("contents[%d].parts[%d] carries code-execution history", contentIndex, partIndex))
			}
		}
	}
	return nil
}

// assistantFunctionHistoryPart 用共享 Content 编解码器编码一个函数调用或结果
// 历史 Part，并核对它只落在已验证的共享 Part 索引上。它只服务 field 1 的
// contents：不写任何顶层 envelope，也不把工具声明塞进历史。
//
// 该原语是为“将来找到声明/选择 envelope 后复用同一套历史编码”准备的；在当前
// 文本阶段，assistantClientToolRefusal 会在进入编码前拒绝所有工具请求。
func assistantFunctionHistoryPart(part Part) ([]any, error) {
	switch {
	case part.FunctionCall != nil && part.FunctionResult != nil:
		return nil, fmt.Errorf("function history part 不能同时包含调用与结果")
	case part.FunctionCall == nil && part.FunctionResult == nil:
		return nil, fmt.Errorf("function history part 必须包含调用或结果")
	}
	wire, err := encodePart(part)
	if err != nil {
		return nil, err
	}
	expected := assistantFunctionCallPartIndex
	label := "functionCall"
	name := ""
	if part.FunctionResult != nil {
		expected = assistantFunctionResponsePartIndex
		label = "functionResponse"
		name = part.FunctionResult.Name
	} else {
		name = part.FunctionCall.Name
	}
	if name == "" {
		return nil, fmt.Errorf("%s history part 缺少函数名", label)
	}
	if expected >= len(wire) || wire[expected] == nil {
		return nil, fmt.Errorf("%s 没有落在已验证的 Part 索引 %d", label, expected)
	}
	// 除函数槽位外，共享 Part 的其它内容变体都不能被这条历史原语带出；
	// 索引 12（thought）与 14（thought signature）不是内容变体，予以保留。
	for _, index := range []int{1, 2, 5, 6, 7, 8, 10, 11, 40} {
		if index == expected {
			continue
		}
		if index < len(wire) && wire[index] != nil {
			return nil, fmt.Errorf("%s 编码写入了未验证的 Part 索引 %d", label, index)
		}
	}
	values, ok := wire[expected].([]any)
	if !ok || len(values) < 2 {
		return nil, fmt.Errorf("%s 编码不是 [name, Struct, id?]", label)
	}
	if got, ok := values[0].(string); !ok || got != name {
		return nil, fmt.Errorf("%s 编码丢失函数名", label)
	}
	return wire, nil
}

// assistantResponseToolSlot 报告一个已解码的助手流 Part 是否占用共享的
// functionCall/functionResponse 槽位。助手响应对这两个槽位没有已验证的投影，
// 调用方必须失败关闭，不能当作文本空成功或静默丢弃。
func assistantResponseToolSlot(part []json.RawMessage) (string, bool) {
	if assistantFunctionCallPartIndex < len(part) && !isJSONNull(part[assistantFunctionCallPartIndex]) {
		return "functionCall", true
	}
	if assistantFunctionResponsePartIndex < len(part) && !isJSONNull(part[assistantFunctionResponsePartIndex]) {
		return "functionResponse", true
	}
	return "", false
}

// assistantResponseToolEvidenceError 构造助手流函数调用/结果槽位的协议证据
// 错误；它明确说“输出映射未实现”，而不是把调用当正文或普通协议损坏。
func assistantResponseToolEvidenceError(path string, label string) error {
	return &ProtocolEvidenceError{Method: assistantStreamMethod, Path: path, Detail: "unmapped assistant Part occupies the shared " + label + " slot; output mapping not implemented"}
}
