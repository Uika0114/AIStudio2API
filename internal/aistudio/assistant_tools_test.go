package aistudio

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// assistantToolTestRequest 构造最小的文本请求；不含任何工具材料。
func assistantToolTestRequest() GenerateRequest {
	return GenerateRequest{
		ID:        "tool-wire-request",
		Model:     "gemini-4-argon",
		System:    "be concise",
		AccountID: "fixture-account",
		Selection: ModelSelection{
			RequestedModel: "assistant/gemini-4-argon",
			BaseModel:      "gemini-4-argon",
			Channel:        ChannelAssistant,
		},
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "What is 6*7?"}}}},
	}
}

func TestAssistantToolWireEvidenceHasOnlyHistoryEncoding(t *testing.T) {
	capabilities := assistantClientToolCapabilitiesForWire()
	if !capabilities.FunctionHistory {
		t.Fatal("shared Content Part 历史编码应当被承认为已具备")
	}
	if capabilities.Declarations || capabilities.ToolChoice || capabilities.ParallelCalls ||
		capabilities.ResponseFunctionCalls || capabilities.TransportTool {
		t.Fatalf("未验证能力被误标为可用: %+v", capabilities)
	}
	if capabilities.clientToolLoop() {
		t.Fatal("仅历史编码不能构成完整客户端工具闭环")
	}
	if assistantContentsFieldNumber != 1 || assistantFunctionCallPartIndex != 10 || assistantFunctionResponsePartIndex != 11 {
		t.Fatalf("已验证索引被改动: contents=%d call=%d result=%d",
			assistantContentsFieldNumber, assistantFunctionCallPartIndex, assistantFunctionResponsePartIndex)
	}
}

func TestAssistantToolRefusalClassifiesMaterial(t *testing.T) {
	boolTrue := true
	cases := map[string]struct {
		mutate func(*GenerateRequest)
		reason string
	}{
		"text": {},
		"none_choice": {
			mutate: func(request *GenerateRequest) { request.Tools.ToolConfig.Mode = "none" },
		},
		"function_declarations": {
			mutate: func(request *GenerateRequest) {
				request.Tools.Functions = []FunctionDeclaration{{Name: "real"}}
			},
			reason: "function declaration",
		},
		"google_tools": {
			mutate: func(request *GenerateRequest) { request.Tools.Google = []string{"google_search"} },
			reason: "Google tools",
		},
		"google_search_options": {
			mutate: func(request *GenerateRequest) {
				request.Tools.GoogleSearch = &GoogleSearchOptions{WebSearch: true}
			},
			reason: "Google tools",
		},
		"auto_choice": {
			mutate: func(request *GenerateRequest) { request.Tools.ToolConfig.Mode = "auto" },
			reason: `tool choice "auto"`,
		},
		"required_choice": {
			mutate: func(request *GenerateRequest) { request.Tools.ToolConfig.Mode = "required" },
			reason: `tool choice "required"`,
		},
		"named_choice": {
			mutate: func(request *GenerateRequest) { request.Tools.ToolConfig.Mode = "any" },
			reason: `tool choice "any"`,
		},
		"allowed_names": {
			mutate: func(request *GenerateRequest) {
				request.Tools.ToolConfig.AllowedFunctionNames = []string{"real"}
			},
			reason: "allowed function names",
		},
		"parallel_calls": {
			mutate: func(request *GenerateRequest) { request.Tools.ToolConfig.ParallelCalls = &boolTrue },
			reason: "parallel tool calls",
		},
		"anti_truncation": {
			mutate: func(request *GenerateRequest) { request.Selection.AntiTruncation = true },
			reason: "anti-truncation",
		},
		"function_call_history": {
			mutate: func(request *GenerateRequest) {
				request.Contents[0].Parts[0] = Part{FunctionCall: &FunctionCall{Name: "real", Arguments: json.RawMessage(`{}`)}}
			},
			reason: "contents[0].parts[0] carries native function history",
		},
		"function_result_history": {
			mutate: func(request *GenerateRequest) {
				request.Contents[0].Parts[0] = Part{FunctionResult: &FunctionResult{Name: "real", Content: json.RawMessage(`{}`)}}
			},
			reason: "contents[0].parts[0] carries native function history",
		},
		"executable_code_history": {
			mutate: func(request *GenerateRequest) {
				request.Contents[0].Parts[0] = Part{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "1"}}
			},
			reason: "code-execution history",
		},
		"code_result_history": {
			mutate: func(request *GenerateRequest) {
				request.Contents[0].Parts[0] = Part{CodeExecutionResult: &CodeExecutionResult{Outcome: "OUTCOME_OK"}}
			},
			reason: "code-execution history",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			request := assistantToolTestRequest()
			if test.mutate != nil {
				test.mutate(&request)
			}
			err := assistantClientToolRefusal(request)
			if test.reason == "" {
				if err != nil {
					t.Fatalf("纯文本/none 请求被拒绝: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("不是参数错误: %v", err)
			}
			if !strings.Contains(err.Error(), "assistant tool protocol mapping not implemented") {
				t.Fatalf("错误没有指明实现缺口: %v", err)
			}
			if !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("错误缺少 %q: %v", test.reason, err)
			}
		})
	}
}

func TestAssistantToolFunctionHistoryPartUsesVerifiedSharedSlots(t *testing.T) {
	call := Part{FunctionCall: &FunctionCall{
		ID:        "call-1",
		Name:      "get_weather",
		Arguments: json.RawMessage(`{"city":"Paris"}`),
	}}
	wire, err := assistantFunctionHistoryPart(call)
	if err != nil {
		t.Fatalf("编码函数调用历史: %v", err)
	}
	if wire[assistantFunctionCallPartIndex] == nil {
		t.Fatal("functionCall 没有落在共享 Part 索引 10")
	}
	values, ok := wire[assistantFunctionCallPartIndex].([]any)
	if !ok || len(values) != 3 {
		t.Fatalf("functionCall 形状不是 [name, Struct, id]: %#v", wire[assistantFunctionCallPartIndex])
	}
	if values[0] != "get_weather" || values[2] != "call-1" {
		t.Fatalf("函数名/调用 ID 丢失: %#v", values)
	}
	if _, ok := values[1].([]any); !ok {
		t.Fatalf("参数没有编码为 Struct 数组: %#v", values[1])
	}

	result := Part{FunctionResult: &FunctionResult{
		ID:      "call-1",
		Name:    "get_weather",
		Content: json.RawMessage(`{"ok":true}`),
	}}
	resultWire, err := assistantFunctionHistoryPart(result)
	if err != nil {
		t.Fatalf("编码函数结果历史: %v", err)
	}
	resultValues, ok := resultWire[assistantFunctionResponsePartIndex].([]any)
	if !ok || len(resultValues) != 3 {
		t.Fatalf("functionResponse 形状不是 [name, Struct, id]: %#v", resultWire[assistantFunctionResponsePartIndex])
	}
	if resultValues[0] != "get_weather" || resultValues[2] != "call-1" {
		t.Fatalf("函数结果名称/调用 ID 丢失: %#v", resultValues)
	}

	signed := call
	signed.FunctionCall = &FunctionCall{ID: "call-2", Name: "get_weather", Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-1"}
	signedWire, err := assistantFunctionHistoryPart(signed)
	if err != nil {
		t.Fatalf("编码带签名的函数调用历史: %v", err)
	}
	if len(signedWire) <= 14 || signedWire[14] != "sig-1" {
		t.Fatalf("thought signature 没有保留在共享 Part 索引 14: %#v", signedWire)
	}
}

func TestAssistantToolFunctionHistoryPartRejectsOtherPartVariants(t *testing.T) {
	cases := map[string]Part{
		"text":              {Text: "hello"},
		"inline_data":       {InlineData: &Blob{MIME: "image/png", Data: []byte("png")}},
		"executable_code":   {ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "1"}},
		"code_result":       {CodeExecutionResult: &CodeExecutionResult{Outcome: "OUTCOME_OK"}},
		"empty_call":        {FunctionCall: &FunctionCall{Arguments: json.RawMessage(`{}`)}},
		"empty_result_name": {FunctionResult: &FunctionResult{Content: json.RawMessage(`{}`)}},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := assistantFunctionHistoryPart(part); err == nil {
				t.Fatalf("%s 不应被函数历史编码器接受", name)
			}
		})
	}
	both := Part{
		FunctionCall:   &FunctionCall{Name: "real", Arguments: json.RawMessage(`{}`)},
		FunctionResult: &FunctionResult{Name: "real", Content: json.RawMessage(`{}`)},
	}
	if _, err := assistantFunctionHistoryPart(both); err == nil {
		t.Fatal("同时包含调用与结果的 Part 不应被接受")
	}
}

func TestAssistantToolFunctionHistoryTravelsInsideSharedContents(t *testing.T) {
	contents := []Content{
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "real", Arguments: json.RawMessage(`{"x":1}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Name: "real", Content: json.RawMessage(`{"ok":true}`)}}}},
	}
	encoded, err := encodeContents(contents)
	if err != nil {
		t.Fatalf("共享 contents 编码拒绝函数历史: %v", err)
	}
	if len(encoded) != 2 {
		t.Fatalf("contents 数量不符: %d", len(encoded))
	}
	for index, expected := range []int{assistantFunctionCallPartIndex, assistantFunctionResponsePartIndex} {
		content, ok := encoded[index].([]any)
		if !ok || len(content) != 2 {
			t.Fatalf("content %d 形状无效: %#v", index, encoded[index])
		}
		parts, ok := content[0].([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("content %d parts 形状无效: %#v", index, content[0])
		}
		part, ok := parts[0].([]any)
		if !ok || len(part) <= expected || part[expected] == nil {
			t.Fatalf("content %d 没有把函数历史放在索引 %d: %#v", index, expected, parts[0])
		}
	}
}

func TestAssistantToolResponseSlotDetectsSharedFunctionParts(t *testing.T) {
	text := []json.RawMessage{nil, json.RawMessage(`"hello"`), nil, nil, nil, nil, nil, nil, nil, nil, nil}
	if label, ok := assistantResponseToolSlot(text); ok {
		t.Fatalf("文本 Part 被当成工具输出: %s", label)
	}
	call := make([]json.RawMessage, 12)
	call[assistantFunctionCallPartIndex] = json.RawMessage(`["real",[["x",[2,"1"]]],"call-1"]`)
	if label, ok := assistantResponseToolSlot(call); !ok || label != "functionCall" {
		t.Fatalf("functionCall 槽位未识别: %q %v", label, ok)
	}
	result := make([]json.RawMessage, 13)
	result[assistantFunctionResponsePartIndex] = json.RawMessage(`["real",[["ok",[4,true]]],"call-1"]`)
	if label, ok := assistantResponseToolSlot(result); !ok || label != "functionResponse" {
		t.Fatalf("functionResponse 槽位未识别: %q %v", label, ok)
	}
	err := assistantResponseToolEvidenceError("$[0][0][0][0]", "functionCall")
	var evidence *ProtocolEvidenceError
	if !errors.As(err, &evidence) {
		t.Fatalf("不是协议证据错误: %v", err)
	}
	if evidence.Method != assistantStreamMethod || !strings.Contains(evidence.Detail, "output mapping not implemented") {
		t.Fatalf("证据错误内容不符: %+v", evidence)
	}
}

func TestAssistantToolTextStageEncoderUsesOnlyVerifiedTopLevelSlots(t *testing.T) {
	request := assistantToolTestRequest()
	body, err := EncodeAssistantRequest(request, GenerationDefaults{}, "applet-1", "REQUEST-ID")
	if err != nil {
		t.Fatalf("编码文本请求: %v", err)
	}
	var wire []json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("请求不是 JSON 数组: %v", err)
	}
	expected := map[int]bool{0: true, 5: true, 7: true, 8: true, 11: true, 13: true, 15: true, 18: true, 28: true}
	if len(wire) != 29 {
		t.Fatalf("顶层槽位数量变化: %d", len(wire))
	}
	for index, raw := range wire {
		empty := isJSONNull(raw)
		if expected[index] && empty {
			t.Fatalf("已验证槽位 %d 为空", index)
		}
		if !expected[index] && !empty {
			t.Fatalf("文本阶段写入了未验证的顶层槽位 %d: %s", index, strings.TrimSpace(string(raw)))
		}
	}
}
