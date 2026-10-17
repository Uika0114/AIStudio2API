package aistudio

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func antiTestPrepare(t *testing.T, request GenerateRequest) (GenerateRequest, *AntiTruncationTransport) {
	t.Helper()
	upstream, transport, err := PrepareAntiTruncation(request)
	if err != nil {
		t.Fatalf("PrepareAntiTruncation 返回错误: %v", err)
	}
	if transport == nil {
		t.Fatal("PrepareAntiTruncation 返回 nil transport")
	}
	return upstream, transport
}

func antiTestCall(name string, arguments string) Event {
	return Event{Kind: EventToolCall, ToolCall: &FunctionCall{Name: name, Arguments: json.RawMessage(arguments)}}
}

func antiTestProject(t *testing.T, transport *AntiTruncationTransport, event Event) []Event {
	t.Helper()
	events, err := transport.Project(event)
	if err != nil {
		t.Fatalf("Project(%s) 返回错误: %v", event.Kind, err)
	}
	return events
}

func TestAntiTruncationPrepareStableName(t *testing.T) {
	upstream, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	if transport.Name() != antiTruncationTransportBaseName {
		t.Fatalf("运输名 = %q, 期望 %q", transport.Name(), antiTruncationTransportBaseName)
	}
	if len(upstream.Tools.Functions) != 1 {
		t.Fatalf("上游函数数量 = %d, 期望 1", len(upstream.Tools.Functions))
	}
	declaration := upstream.Tools.Functions[0]
	if declaration.Name != antiTruncationTransportBaseName {
		t.Fatalf("声明名 = %q", declaration.Name)
	}
	if !strings.Contains(string(declaration.Parameters), `"required":["content"]`) {
		t.Fatalf("运输 schema 缺少 required content: %s", declaration.Parameters)
	}
	if !strings.Contains(declaration.Description, "no external action") {
		t.Fatalf("运输说明缺少固定控制语: %q", declaration.Description)
	}
}

func TestAntiTruncationPrepareCollisionSuffix(t *testing.T) {
	request := GenerateRequest{Tools: Tools{
		ToolConfig: ToolConfig{Mode: "auto"},
		Functions:  []FunctionDeclaration{{Name: antiTruncationTransportBaseName}},
	}}
	_, transport := antiTestPrepare(t, request)
	if transport.Name() != antiTruncationTransportBaseName+"_2" {
		t.Fatalf("首次避碰名 = %q, 期望 _2", transport.Name())
	}

	request = GenerateRequest{Tools: Tools{
		ToolConfig: ToolConfig{Mode: "auto"},
		Functions: []FunctionDeclaration{
			{Name: antiTruncationTransportBaseName},
			{Name: antiTruncationTransportBaseName + "_2"},
			{Name: antiTruncationTransportBaseName + "_3"},
		},
	}}
	_, transport = antiTestPrepare(t, request)
	if transport.Name() != antiTruncationTransportBaseName+"_4" {
		t.Fatalf("连续避碰名 = %q, 期望 _4", transport.Name())
	}
}

func TestAntiTruncationPrepareDeterministic(t *testing.T) {
	request := GenerateRequest{Tools: Tools{
		ToolConfig: ToolConfig{Mode: "auto"},
		Functions:  []FunctionDeclaration{{Name: antiTruncationTransportBaseName}},
	}}
	_, first := antiTestPrepare(t, request)
	_, second := antiTestPrepare(t, request)
	if first.Name() != second.Name() {
		t.Fatalf("两次准备得到不同运输名: %q vs %q", first.Name(), second.Name())
	}
}

func TestAntiTruncationPrepareDoesNotMutateOriginal(t *testing.T) {
	functions := make([]FunctionDeclaration, 1, 4)
	functions[0] = FunctionDeclaration{Name: "real", Parameters: json.RawMessage(`{"type":"object"}`)}
	request := GenerateRequest{
		System:   "original system",
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
		Tools: Tools{
			Functions: functions,
			Google:    []string{"google_search"},
			ToolConfig: ToolConfig{
				Mode:                 "auto",
				AllowedFunctionNames: []string{"real"},
			},
		},
		SafetySettings: []SafetySetting{{Category: "HARM_CATEGORY", Threshold: "BLOCK_NONE"}},
	}

	upstream, _ := antiTestPrepare(t, request)

	if request.System != "original system" {
		t.Fatalf("原 system 被修改: %q", request.System)
	}
	if len(request.Tools.Functions) != 1 || request.Tools.Functions[0].Name != "real" {
		t.Fatalf("原函数列表被修改: %+v", request.Tools.Functions)
	}
	if string(request.Tools.Functions[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("原参数被修改: %s", request.Tools.Functions[0].Parameters)
	}
	if len(request.Tools.Google) != 1 || request.Tools.Google[0] != "google_search" {
		t.Fatalf("原 Google 工具被修改: %+v", request.Tools.Google)
	}
	if len(request.Tools.ToolConfig.AllowedFunctionNames) != 1 {
		t.Fatalf("原 allowlist 被修改: %+v", request.Tools.ToolConfig.AllowedFunctionNames)
	}
	if len(request.SafetySettings) != 1 {
		t.Fatalf("原安全设置被修改: %+v", request.SafetySettings)
	}

	// 修改副本不得回写原请求。
	upstream.Tools.Functions[0].Name = "mutated"
	upstream.Tools.Google[0] = "mutated"
	upstream.Tools.ToolConfig.AllowedFunctionNames[0] = "mutated"
	upstream.SafetySettings[0].Category = "mutated"
	if request.Tools.Functions[0].Name != "real" ||
		request.Tools.Google[0] != "google_search" ||
		request.Tools.ToolConfig.AllowedFunctionNames[0] != "real" ||
		request.SafetySettings[0].Category != "HARM_CATEGORY" {
		t.Fatal("修改上游副本回写到了原请求")
	}
}

func TestAntiTruncationPrepareModeNoneDropsRealTools(t *testing.T) {
	request := GenerateRequest{Tools: Tools{
		Functions:    []FunctionDeclaration{{Name: "real"}},
		Google:       []string{"google_search"},
		GoogleSearch: &GoogleSearchOptions{WebSearch: true},
		ToolConfig:   ToolConfig{Mode: "none"},
	}}
	upstream, transport := antiTestPrepare(t, request)
	if len(upstream.Tools.Functions) != 1 || upstream.Tools.Functions[0].Name != transport.Name() {
		t.Fatalf("none 模式上游函数 = %+v, 期望只有运输", upstream.Tools.Functions)
	}
	if upstream.Tools.Google != nil || upstream.Tools.GoogleSearch != nil {
		t.Fatalf("none 模式仍暴露内置工具: %+v", upstream.Tools)
	}
	if upstream.Tools.ToolConfig.Mode != "required" || !reflect.DeepEqual(upstream.Tools.ToolConfig.AllowedFunctionNames, []string{transport.Name()}) {
		t.Fatalf("none must allow only the hidden transport: %+v", upstream.Tools.ToolConfig)
	}
	if request.Tools.ToolConfig.Mode != "none" {
		t.Fatal("public none contract changed")
	}
}

func TestAntiTruncationPrepareModeAutoKeepsRealTools(t *testing.T) {
	request := GenerateRequest{Tools: Tools{
		Functions:    []FunctionDeclaration{{Name: "real"}},
		GoogleSearch: &GoogleSearchOptions{WebSearch: true},
		ToolConfig:   ToolConfig{Mode: "auto"},
	}}
	upstream, transport := antiTestPrepare(t, request)
	if len(upstream.Tools.Functions) != 2 {
		t.Fatalf("auto 模式函数数量 = %d, 期望 2", len(upstream.Tools.Functions))
	}
	if upstream.Tools.Functions[0].Name != "real" || upstream.Tools.Functions[1].Name != transport.Name() {
		t.Fatalf("auto 模式函数顺序错误: %+v", upstream.Tools.Functions)
	}
	if upstream.Tools.GoogleSearch == nil || !upstream.Tools.GoogleSearch.WebSearch {
		t.Fatalf("auto 模式丢失内置工具: %+v", upstream.Tools)
	}
	if upstream.Tools.ToolConfig.Mode != "required" || !reflect.DeepEqual(upstream.Tools.ToolConfig.AllowedFunctionNames, []string{"real", transport.Name()}) {
		t.Fatalf("wire auto must select a real call or transported text: %+v", upstream.Tools.ToolConfig)
	}
	if request.Tools.ToolConfig.Mode != "auto" {
		t.Fatal("public auto contract changed")
	}
}

func TestAntiTruncationPrepareModeRequiredKeepsClientList(t *testing.T) {
	inputSystem := "Use one of these tools in this response: real_one, real_two. Use the tool before answering."
	request := GenerateRequest{System: inputSystem, Tools: Tools{
		Functions:  []FunctionDeclaration{{Name: "real_one"}, {Name: "real_two"}},
		ToolConfig: ToolConfig{Mode: "required"},
	}}
	upstream, transport := antiTestPrepare(t, request)
	if upstream.Tools.ToolConfig.Mode != "required" {
		t.Fatalf("required 模式被改写为 %q", upstream.Tools.ToolConfig.Mode)
	}
	if !strings.HasPrefix(upstream.System, inputSystem+"\n") {
		t.Fatalf("required 提示被破坏: %q", upstream.System)
	}
	tail := strings.TrimPrefix(upstream.System, inputSystem+"\n")
	if tail != fmt.Sprintf(antiTruncationSystemInstruction, transport.Name()) {
		t.Fatalf("运输控制提示错误: %q", tail)
	}
	if strings.Contains(inputSystem, transport.Name()) {
		t.Fatal("运输函数进入了 required 真实工具列表")
	}
}

func TestAntiTruncationPrepareModeValidatedAndAllowlistAndParallel(t *testing.T) {
	parallel := false
	request := GenerateRequest{Tools: Tools{
		Functions: []FunctionDeclaration{{Name: "real_one", Strict: true}},
		ToolConfig: ToolConfig{
			Mode:                 "validated",
			AllowedFunctionNames: []string{"real_one"},
			ParallelCalls:        &parallel,
		},
	}}
	upstream, transport := antiTestPrepare(t, request)
	if upstream.Tools.ToolConfig.Mode != "required" {
		t.Fatalf("wire mode = %q", upstream.Tools.ToolConfig.Mode)
	}
	if !reflect.DeepEqual(upstream.Tools.ToolConfig.AllowedFunctionNames, []string{"real_one", transport.Name()}) {
		t.Fatalf("wire allowlist does not preserve real choice plus text transport: %+v", upstream.Tools.ToolConfig.AllowedFunctionNames)
	}
	if upstream.Tools.ToolConfig.ParallelCalls == nil || *upstream.Tools.ToolConfig.ParallelCalls {
		t.Fatal("parallel 约束被改写")
	}
	if len(upstream.Tools.Functions) != 2 || upstream.Tools.Functions[1].Name != transport.Name() {
		t.Fatalf("allowlist 模式函数 = %+v", upstream.Tools.Functions)
	}
	if request.Tools.ToolConfig.Mode != "validated" || !reflect.DeepEqual(request.Tools.ToolConfig.AllowedFunctionNames, []string{"real_one"}) {
		t.Fatal("original client contract was mutated")
	}
}

func TestAntiTruncationPrepareUnknownMode(t *testing.T) {
	_, transport, err := PrepareAntiTruncation(GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "made-up"}}})
	if !errors.Is(err, errAntiTruncationToolChoice) {
		t.Fatalf("未知模式错误 = %v", err)
	}
	if transport != nil {
		t.Fatal("未知模式返回了非 nil transport")
	}
}

func TestAntiTruncationProjectTransportToText(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	events := antiTestProject(t, transport, Event{
		Kind:          EventToolCall,
		ProviderModel: "gemini-x",
		ToolCall:      &FunctionCall{ID: "internal-1", Name: transport.Name(), Arguments: json.RawMessage(`{"content":"hello world"}`), ThoughtSignature: "internal-signature"},
	})
	if len(events) != 1 || events[0].Kind != EventText || events[0].Text != "hello world" {
		t.Fatalf("运输投影 = %+v", events)
	}
	if events[0].ThoughtSignature != "" {
		t.Fatalf("运输签名泄露到正文事件: %q", events[0].ThoughtSignature)
	}
	if events[0].ProviderModel != "gemini-x" {
		t.Fatalf("provider model 丢失: %q", events[0].ProviderModel)
	}
	finish := antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: "stop"})
	if len(finish) != 1 || finish[0].FinishReason != "stop" {
		t.Fatalf("终态投影 = %+v", finish)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestAntiTruncationProjectUnicodeAndEscapes(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	events := antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":"\u4e2d\u6587\n\"quoted\"\tend"}`))
	if len(events) != 1 {
		t.Fatalf("Unicode 投影数量 = %d", len(events))
	}
	want := "中文\n\"quoted\"\tend"
	if events[0].Text != want {
		t.Fatalf("Unicode 正文 = %q, 期望 %q", events[0].Text, want)
	}
}

func TestAntiTruncationProjectEmptyContent(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, Event{Kind: EventText, Text: "ordinary"})
	events := antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":""}`))
	if len(events) != 0 {
		t.Fatalf("空 content 产生了事件: %+v", events)
	}
	finish := antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: "stop"})
	if len(finish) != 1 || finish[0].Kind != EventFinish {
		t.Fatalf("空 content 终态 = %+v", finish)
	}
}

func TestAntiTruncationProjectBrokenArguments(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      error
	}{
		{"空", "", errAntiTruncationArguments},
		{"未闭合", `{`, errAntiTruncationArguments},
		{"数组", `[]`, errAntiTruncationArguments},
		{"字符串", `"x"`, errAntiTruncationArguments},
		{"content 数字", `{"content":123}`, errAntiTruncationContentType},
		{"content null", `{"content":null}`, errAntiTruncationContentType},
		{"content 对象", `{"content":{"a":1}}`, errAntiTruncationContentType},
		{"缺 content", `{"other":"x"}`, errAntiTruncationMissingContent},
		{"重复 content", `{"content":"a","content":"b"}`, errAntiTruncationDuplicateContent},
		{"尾随文本", `{"content":"a"} trailing`, errAntiTruncationTrailing},
		{"尾随对象", `{"content":"a"} {"x":1}`, errAntiTruncationTrailing},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
			_, err := transport.Project(antiTestCall(transport.Name(), testCase.arguments))
			if !errors.Is(err, testCase.want) {
				t.Fatalf("错误 = %v, 期望 %v", err, testCase.want)
			}
		})
	}
}

func TestAntiTruncationProjectOrdinaryFallback(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	if events := antiTestProject(t, transport, Event{Kind: EventText, Text: "hello ", ProviderModel: "gemini-x"}); len(events) != 0 {
		t.Fatalf("普通正文应暂存, 得到 %+v", events)
	}
	if events := antiTestProject(t, transport, Event{Kind: EventText, Text: "world"}); len(events) != 0 {
		t.Fatalf("普通正文应暂存, 得到 %+v", events)
	}
	events := antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: "stop"})
	if len(events) != 2 {
		t.Fatalf("fallback 事件 = %+v", events)
	}
	if events[0].Kind != EventText || events[0].Text != "hello world" || events[0].ProviderModel != "gemini-x" {
		t.Fatalf("fallback 正文 = %+v", events[0])
	}
	if events[1].Kind != EventFinish {
		t.Fatalf("fallback 终态 = %+v", events[1])
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestAntiTruncationProjectTransportSuppressesOrdinary(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, Event{Kind: EventText, Text: "truncated-ordinary"})
	events := antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":"complete-final"}`))
	if len(events) != 1 || events[0].Text != "complete-final" {
		t.Fatalf("运输正文 = %+v", events)
	}
	finish := antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: "stop"})
	if len(finish) != 1 || finish[0].Kind != EventFinish {
		t.Fatalf("终态 = %+v", finish)
	}
}

func TestAntiTruncationProjectTextAfterTransport(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":"final"}`))
	if _, err := transport.Project(Event{Kind: EventText, Text: "extra"}); !errors.Is(err, errAntiTruncationTextAfterCall) {
		t.Fatalf("运输后普通正文错误 = %v", err)
	}
}

func TestAntiTruncationProjectDuplicateTransport(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":"first"}`))
	if _, err := transport.Project(antiTestCall(transport.Name(), `{"content":"second"}`)); !errors.Is(err, errAntiTruncationDuplicateCall) {
		t.Fatalf("重复运输错误 = %v", err)
	}
}

func TestAntiTruncationProjectRealToolPreserved(t *testing.T) {
	request := GenerateRequest{Tools: Tools{
		Functions:  []FunctionDeclaration{{Name: "real_tool"}},
		ToolConfig: ToolConfig{Mode: "auto"},
	}}
	_, transport := antiTestPrepare(t, request)
	call := &FunctionCall{ID: "call_1", Name: "real_tool", Arguments: json.RawMessage(`{"x":1}`), ThoughtSignature: "real-signature"}
	events := antiTestProject(t, transport, Event{Kind: EventToolCall, ToolCall: call, ThoughtSignature: "real-signature"})
	if len(events) != 1 || events[0].Kind != EventToolCall {
		t.Fatalf("真实工具投影 = %+v", events)
	}
	if events[0].ToolCall == nil || events[0].ToolCall.ID != "call_1" || events[0].ToolCall.Name != "real_tool" ||
		string(events[0].ToolCall.Arguments) != `{"x":1}` || events[0].ToolCall.ThoughtSignature != "real-signature" {
		t.Fatalf("真实工具语义被改写: %+v", events[0].ToolCall)
	}
}

func TestAntiTruncationProjectReasoningUsageSignatureForwarded(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	events := antiTestProject(t, transport, Event{Kind: EventReasoning, Text: "thinking"})
	if len(events) != 1 || events[0].Kind != EventReasoning {
		t.Fatalf("reasoning 投影 = %+v", events)
	}
	usage := &Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}
	events = antiTestProject(t, transport, Event{Kind: EventUsage, Usage: usage})
	if len(events) != 1 || events[0].Usage != usage {
		t.Fatalf("usage 投影 = %+v", events)
	}
	events = antiTestProject(t, transport, Event{Kind: EventThoughtSignature, ThoughtSignature: "standalone"})
	if len(events) != 1 || events[0].ThoughtSignature != "standalone" {
		t.Fatalf("独立签名投影 = %+v", events)
	}
}

func TestAntiTruncationProjectBufferLimit(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	transport.textLimit = 4
	if _, err := transport.Project(Event{Kind: EventText, Text: "12345"}); !errors.Is(err, errAntiTruncationTextLimit) {
		t.Fatalf("超限错误 = %v", err)
	}
	transport.textLimit = 4
	antiTestProject(t, transport, Event{Kind: EventText, Text: "1234"})
	if _, err := transport.Project(Event{Kind: EventText, Text: "5"}); !errors.Is(err, errAntiTruncationTextLimit) {
		t.Fatalf("累积超限错误 = %v", err)
	}
}

func TestAntiTruncationProjectErrorPreserved(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, Event{Kind: EventText, Text: "partial"})
	upstreamErr := errors.New("upstream failed")
	events := antiTestProject(t, transport, Event{Kind: EventError, Err: upstreamErr})
	if len(events) != 1 || events[0].Kind != EventError || !errors.Is(events[0].Err, upstreamErr) {
		t.Fatalf("错误投影 = %+v", events)
	}
	if _, err := transport.Project(Event{Kind: EventFinish, FinishReason: "stop"}); !errors.Is(err, errAntiTruncationAfterTerminal) {
		t.Fatalf("错误后 finish 错误 = %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("错误终态后 Close = %v", err)
	}
}

func TestAntiTruncationProjectTrailingUsageAfterTerminal(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":"final"}`))
	antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: "stop"})
	usage := &Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}
	events := antiTestProject(t, transport, Event{Kind: EventUsage, Usage: usage})
	if len(events) != 1 || events[0].Usage != usage {
		t.Fatalf("尾随 usage 投影 = %+v", events)
	}
	if _, err := transport.Project(Event{Kind: EventText, Text: "late"}); !errors.Is(err, errAntiTruncationAfterTerminal) {
		t.Fatalf("终态后正文错误 = %v", err)
	}
}

func TestAntiTruncationProjectFinishReasonsPreserved(t *testing.T) {
	for _, reason := range []string{"max_tokens", "safety", "recitation"} {
		_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
		events := antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: reason})
		if len(events) != 1 || events[0].FinishReason != reason {
			t.Fatalf("终态 %q 被改写为 %+v", reason, events)
		}
	}
}

func TestAntiTruncationCloseMissingTerminal(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, Event{Kind: EventText, Text: "no finish"})
	if err := transport.Close(); !errors.Is(err, errAntiTruncationMissingTerminal) {
		t.Fatalf("缺终态 Close = %v", err)
	}
	if err := transport.Close(); !errors.Is(err, errAntiTruncationMissingTerminal) {
		t.Fatalf("重复 Close 应返回同一错误: %v", err)
	}
	if _, err := transport.Project(Event{Kind: EventText, Text: "late"}); !errors.Is(err, errAntiTruncationClosed) {
		t.Fatalf("Close 后 Project 错误 = %v", err)
	}
}

func TestAntiTruncationCloseAfterFinish(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	antiTestProject(t, transport, Event{Kind: EventFinish, FinishReason: "stop"})
	if err := transport.Close(); err != nil {
		t.Fatalf("正常终态 Close = %v", err)
	}
}

func TestAntiTruncationNilTransport(t *testing.T) {
	var transport *AntiTruncationTransport
	if transport.Name() != "" {
		t.Fatal("nil transport Name 应为空")
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("nil transport Close = %v", err)
	}
	if _, err := transport.Project(Event{Kind: EventText, Text: "x"}); !errors.Is(err, errAntiTruncationClosed) {
		t.Fatalf("nil transport Project = %v", err)
	}
}

func TestAntiTruncationMultiInstanceIsolation(t *testing.T) {
	_, first := antiTestPrepare(t, GenerateRequest{Tools: Tools{
		Functions:  []FunctionDeclaration{{Name: antiTruncationTransportBaseName}},
		ToolConfig: ToolConfig{Mode: "auto"},
	}})
	_, second := antiTestPrepare(t, GenerateRequest{Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}})
	if first.Name() == second.Name() {
		t.Fatalf("两个实例运输名相同: %q", first.Name())
	}

	antiTestProject(t, first, Event{Kind: EventText, Text: "first-buffer"})
	secondEvents := antiTestProject(t, second, antiTestCall(second.Name(), `{"content":"second-final"}`))
	if len(secondEvents) != 1 || secondEvents[0].Text != "second-final" {
		t.Fatalf("第二实例运输 = %+v", secondEvents)
	}

	firstFinish := antiTestProject(t, first, Event{Kind: EventFinish, FinishReason: "stop"})
	if len(firstFinish) != 2 || firstFinish[0].Text != "first-buffer" {
		t.Fatalf("第一实例 fallback = %+v", firstFinish)
	}
	secondFinish := antiTestProject(t, second, Event{Kind: EventFinish, FinishReason: "stop"})
	if len(secondFinish) != 1 || secondFinish[0].Kind != EventFinish {
		t.Fatalf("第二实例终态 = %+v", secondFinish)
	}
}
