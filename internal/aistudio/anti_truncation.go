package aistudio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// 本文件实现首期“完整参数工具抗截断”的纯请求/事件转换层。
//
// 调用合同（由主 Agent 接线，本文件不改动 types.go、GenerateRequest 或工具合同）：
//   - 调用方先完成模型能力核验、supportedTools 与 prepareToolRequest，得到已反映
//     客户端真实工具合同的请求与 *toolContract；随后对本模块调用
//     PrepareAntiTruncation，得到独立的上游请求副本与本请求私有的
//     *AntiTruncationTransport。
//   - 上游事件先经 transport.Project 投影，再把投影后的事件交给原 client
//     contract（真实工具合同）与 stop 处理；投影会吞掉内部运输调用，因此真实工具
//     合同只能由真实工具满足。
//   - 流结束后调用 transport.Close 做终态完整性检查。
//
// 本模块不发送请求、不重试、不新增生成、不写 chan/goroutine，也不重新推算 usage。

// antiTruncationTransportBaseName 是内部运输函数的稳定名称，冲突时确定性追加 _2/_3。
const antiTruncationTransportBaseName = "aistudio2api_emit_v1"

// antiTruncationTransportDescription 是运输函数声明的固定说明，不随请求变化。
const antiTruncationTransportDescription = "Internal transport for the complete final answer only. Call this function once with the full final answer text in the \"content\" string argument. It performs no external action and must never be used for any other purpose."

// antiTruncationSystemInstruction 是注入的固定控制提示模板，只有函数名随避碰结果变化。
const antiTruncationSystemInstruction = "Honor the caller requirements for real tools first; the internal transport cannot satisfy a required real tool call. When a final answer is appropriate, deliver its complete text by calling the internal function %q exactly once, placing the full final answer in its \"content\" string argument. That function only transports text and performs no external action; do not use it for any other purpose and do not call it more than once."

// antiTruncationTextBufferLimit 是普通正文的有界暂存上限；超过即显式报错，不截断后假成功。
const antiTruncationTextBufferLimit = 4 << 20

var (
	errAntiTruncationToolChoice       = errors.New("抗截断不支持的工具选择模式")
	errAntiTruncationArguments        = errors.New("抗截断运输调用参数不是合法 JSON 对象")
	errAntiTruncationMissingContent   = errors.New("抗截断运输调用缺少 content")
	errAntiTruncationContentType      = errors.New("抗截断运输调用 content 必须是字符串")
	errAntiTruncationDuplicateContent = errors.New("抗截断运输调用包含重复 content")
	errAntiTruncationTrailing         = errors.New("抗截断运输调用参数包含多余内容")
	errAntiTruncationDuplicateCall    = errors.New("同一候选重复出现抗截断运输调用")
	errAntiTruncationTextAfterCall    = errors.New("抗截断运输正文之后又出现普通正文")
	errAntiTruncationTextLimit        = errors.New("普通正文缓冲超过限额")
	errAntiTruncationMissingTerminal  = errors.New("流结束前没有真实终态")
	errAntiTruncationAfterTerminal    = errors.New("终态之后出现非 usage 事件")
	errAntiTruncationClosed           = errors.New("抗截断运输已经关闭")
)

// AntiTruncationTransport 保存单个请求/attempt/candidate 的运输解包状态。
//
// 一个实例只对应一个规范生成/choice；不跨 candidate/call 拼正文，也不以稳定函数名
// 作为跨请求共享缓存键。实例不并发安全，调用方按事件顺序串行使用。
type AntiTruncationTransport struct {
	name      string
	textLimit int

	buffer      strings.Builder
	bufferModel string

	hit      bool
	terminal bool
	closed   bool
	closeErr error
}

// Name 返回本次注入的运输函数名，供调用方日志与合同审计使用。
func (transport *AntiTruncationTransport) Name() string {
	if transport == nil {
		return ""
	}
	return transport.name
}

// PrepareAntiTruncation 返回注入内部运输函数后的独立上游请求副本与本请求私有状态。
//
// 调用方已经决定启用 anti，本函数不读取也不修改任何 anti 策略字段。原请求的
// system/tools/嵌套参数与 history 不被原地修改：Tools、Contents、SafetySettings
// 都在副本上重建。
//
// 客户端真实工具合同与内部运输分离：
//   - mode 为 none 时，客户端真实工具与内置工具一律不进入上游请求，只保留内部运输，
//     只强制内部运输；公开投影仍是文本，客户端 none 语义不变。
//   - mode 为 auto/空/required/validated 时，真实工具声明保持不变，运输作为额外声明
//     追加；required/allowlist/parallel 语义保留在真实工具上，运输不进入它们的必选列表。
//   - 同名客户端工具通过确定性避碰（_2/_3）保留原名，运输只吞本次注入的 exact 名称。
func PrepareAntiTruncation(request GenerateRequest) (GenerateRequest, *AntiTruncationTransport, error) {
	mode := request.Tools.ToolConfig.Mode
	switch mode {
	case "", "auto", "required", "validated", "none":
	default:
		return request, nil, fmt.Errorf("%w: %q", errAntiTruncationToolChoice, mode)
	}

	upstream := request
	upstream.System = request.System
	upstream.Contents = cloneContentsForFileCopies(request.Contents)
	upstream.SafetySettings = append([]SafetySetting(nil), request.SafetySettings...)
	upstream.Tools = cloneToolsForAntiTruncation(request.Tools)

	taken := make(map[string]bool, len(request.Tools.Functions))
	for _, declaration := range request.Tools.Functions {
		taken[declaration.Name] = true
	}
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				taken[part.FunctionCall.Name] = true
			}
			if part.FunctionResult != nil {
				taken[part.FunctionResult.Name] = true
			}
		}
	}
	name := antiTruncationCollisionFreeName(antiTruncationTransportBaseName, taken)

	if mode == "none" {
		// 客户端声明了 none：不得向上游暴露任何客户端真实工具或内置工具。
		upstream.Tools = Tools{Functions: []FunctionDeclaration{antiTruncationTransportDeclaration(name)}}
	} else {
		// An empty required allowlist originally meant any REAL function. Adding
		// the transport declaration must not change that meaning to include it.
		if mode == "required" && len(upstream.Tools.ToolConfig.AllowedFunctionNames) == 0 {
			for _, declaration := range request.Tools.Functions {
				upstream.Tools.ToolConfig.AllowedFunctionNames = append(upstream.Tools.ToolConfig.AllowedFunctionNames, declaration.Name)
			}
		}
		upstream.Tools.Functions = append(upstream.Tools.Functions, antiTruncationTransportDeclaration(name))
	}
	if mode != "required" {
		// ANY covers either an allowed real call or the hidden text transport.
		// Public auto/none/validated behavior is enforced by the ORIGINAL client
		// contract after projection, not by counting this internal function.
		upstream.Tools.ToolConfig.Mode = "required"
		allowed := append([]string(nil), upstream.Tools.ToolConfig.AllowedFunctionNames...)
		if len(allowed) == 0 {
			for _, declaration := range upstream.Tools.Functions {
				allowed = append(allowed, declaration.Name)
			}
		} else {
			allowed = append(allowed, name)
		}
		upstream.Tools.ToolConfig.AllowedFunctionNames = allowed
	}
	upstream.System = appendSystemInstruction(upstream.System, fmt.Sprintf(antiTruncationSystemInstruction, name))

	return upstream, &AntiTruncationTransport{name: name, textLimit: antiTruncationTextBufferLimit}, nil
}

// Project 把一条解码后的完整参数事件投影为零个或多个普通事件。
//
// 运输调用（仅本次注入的 exact 名称）解包为普通文本；真实工具、思考、签名、usage 与
// 终态事件按合同保留。普通正文有界暂存，运输命中后不再交付先前普通正文，合法结束且
// 没有运输时 fallback 普通正文。运输解包失败直接返回错误，不 fallback 掩盖。
func (transport *AntiTruncationTransport) Project(event Event) ([]Event, error) {
	if transport == nil {
		return nil, errAntiTruncationClosed
	}
	if transport.closed {
		return nil, errAntiTruncationClosed
	}
	if transport.terminal {
		// 真实协议可能在终态后补一条 usage；保留它，其余一律视为越界。
		if event.Kind == EventUsage {
			return []Event{event}, nil
		}
		return nil, fmt.Errorf("%w: %s", errAntiTruncationAfterTerminal, event.Kind)
	}

	switch event.Kind {
	case EventText:
		return transport.projectText(event)
	case EventToolCall:
		return transport.projectToolCall(event)
	case EventFinish:
		transport.terminal = true
		if transport.hit {
			return []Event{event}, nil
		}
		if flushed := transport.flush(); flushed != nil {
			return []Event{*flushed, event}, nil
		}
		return []Event{event}, nil
	case EventError:
		// 保留错误语义，不假造 finish。
		transport.terminal = true
		return []Event{event}, nil
	default:
		// reasoning、真实工具（非运输）、签名、usage、媒体、来源等按合同原样保留。
		return []Event{event}, nil
	}
}

// Close 校验流在结束后出现过真实终态；缺终态的 EOF 不算成功。
func (transport *AntiTruncationTransport) Close() error {
	if transport == nil {
		return nil
	}
	if transport.closed {
		return transport.closeErr
	}
	transport.closed = true
	if !transport.terminal {
		transport.closeErr = errAntiTruncationMissingTerminal
	}
	return transport.closeErr
}

func (transport *AntiTruncationTransport) projectText(event Event) ([]Event, error) {
	if event.Text == "" {
		return nil, nil
	}
	if transport.hit {
		return nil, errAntiTruncationTextAfterCall
	}
	if transport.buffer.Len()+len(event.Text) > transport.textLimit {
		return nil, errAntiTruncationTextLimit
	}
	transport.buffer.WriteString(event.Text)
	if event.ProviderModel != "" {
		transport.bufferModel = event.ProviderModel
	}
	return nil, nil
}

func (transport *AntiTruncationTransport) projectToolCall(event Event) ([]Event, error) {
	call := event.ToolCall
	if call == nil || call.Name != transport.name {
		// 真实工具调用原样转发，签名与 ID 语义不变。
		return []Event{event}, nil
	}
	if transport.hit {
		return nil, errAntiTruncationDuplicateCall
	}
	content, err := decodeAntiTruncationContent(call.Arguments)
	if err != nil {
		return nil, fmt.Errorf("抗截断运输 %q: %w", transport.name, err)
	}
	transport.hit = true
	transport.buffer.Reset()
	if content == "" {
		// 空 content 如实处理：不补假正文，也不重试。
		return nil, nil
	}
	// 运输调用的 ID 与附带签名不泄露给客户端，只投影为普通正文。
	return []Event{{Kind: EventText, Text: content, ProviderModel: event.ProviderModel}}, nil
}

func (transport *AntiTruncationTransport) flush() *Event {
	if transport.buffer.Len() == 0 {
		return nil
	}
	text := transport.buffer.String()
	transport.buffer.Reset()
	return &Event{Kind: EventText, Text: text, ProviderModel: transport.bufferModel}
}

// decodeAntiTruncationContent 严格解包运输调用的 content 字段。
func decodeAntiTruncationContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", errAntiTruncationArguments
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return "", errAntiTruncationArguments
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return "", errAntiTruncationArguments
	}
	content := ""
	seen := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", errAntiTruncationArguments
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", errAntiTruncationArguments
		}
		if key != "content" {
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return "", errAntiTruncationArguments
			}
			continue
		}
		if seen {
			return "", errAntiTruncationDuplicateContent
		}
		seen = true
		valueToken, err := decoder.Token()
		if err != nil {
			return "", errAntiTruncationArguments
		}
		value, ok := valueToken.(string)
		if !ok {
			return "", errAntiTruncationContentType
		}
		content = value
	}
	if _, err := decoder.Token(); err != nil {
		return "", errAntiTruncationArguments
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", errAntiTruncationTrailing
	}
	if !seen {
		return "", errAntiTruncationMissingContent
	}
	return content, nil
}

// antiTruncationTransportDeclaration 构造稳定 schema 的运输函数声明。
func antiTruncationTransportDeclaration(name string) FunctionDeclaration {
	return FunctionDeclaration{
		Name:        name,
		Description: antiTruncationTransportDescription,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"The complete final answer text."}},"required":["content"]}`),
	}
}

// antiTruncationCollisionFreeName 从 base 开始确定性避碰：冲突时依次尝试 base_2、base_3……
func antiTruncationCollisionFreeName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "_" + strconv.Itoa(suffix)
		if !taken[candidate] {
			return candidate
		}
	}
}

// appendSystemInstruction 在保留原 system 的前提下追加固定控制提示。
func appendSystemInstruction(system string, instruction string) string {
	if strings.TrimSpace(system) == "" {
		return instruction
	}
	return system + "\n" + instruction
}

// cloneToolsForAntiTruncation 复制工具集合，保证追加运输声明不会写回原请求的 slice/map。
func cloneToolsForAntiTruncation(tools Tools) Tools {
	cloned := tools
	if tools.Functions != nil {
		cloned.Functions = make([]FunctionDeclaration, len(tools.Functions))
		for index, declaration := range tools.Functions {
			cloned.Functions[index] = declaration
			if declaration.Parameters != nil {
				cloned.Functions[index].Parameters = append(json.RawMessage(nil), declaration.Parameters...)
			}
		}
	}
	if tools.Google != nil {
		cloned.Google = append([]string(nil), tools.Google...)
	}
	if search := tools.GoogleSearch; search != nil {
		copied := *search
		if search.AllowedDomains != nil {
			copied.AllowedDomains = append([]string(nil), search.AllowedDomains...)
		}
		if search.UserLocation != nil {
			copied.UserLocation = append(json.RawMessage(nil), search.UserLocation...)
		}
		if search.TimeRange != nil {
			timeRange := *search.TimeRange
			copied.TimeRange = &timeRange
		}
		cloned.GoogleSearch = &copied
	}
	if tools.ToolConfig.AllowedFunctionNames != nil {
		cloned.ToolConfig.AllowedFunctionNames = append([]string(nil), tools.ToolConfig.AllowedFunctionNames...)
	}
	if tools.ToolConfig.ParallelCalls != nil {
		parallel := *tools.ToolConfig.ParallelCalls
		cloned.ToolConfig.ParallelCalls = &parallel
	}
	return cloned
}
