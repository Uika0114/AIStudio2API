package aistudio

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 本文件只覆盖 prepareAssistantTextRequest 的纯函数行为：不创建 Applet、不发
// RPC、不访问账户/凭据/模型 API。所有输入都是合成 fixture。

func assistantDowngradeRequest() GenerateRequest {
	return GenerateRequest{
		ID:        "downgrade-fixture",
		Model:     "gemini-4-argon",
		System:    "be concise",
		AccountID: "fixture-account",
		Selection: ModelSelection{
			RequestedModel: "assistant/gemini-4-argon",
			BaseModel:      "gemini-4-argon",
			Channel:        ChannelAssistant,
		},
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}}},
	}
}

type assistantDowngradeFingerprint struct {
	Request   GenerateRequest `json:"request"`
	Selection ModelSelection  `json:"selection"`
}

// assistantDowngradeFingerprintOf 把请求（含被 json:"-" 排除的 Selection）序列化，
// 用来证明预处理没有原地改动调用方传入的任何内容，包括指针目标与 RawMessage。
func assistantDowngradeFingerprintOf(t *testing.T, request GenerateRequest) string {
	t.Helper()
	payload, err := json.Marshal(assistantDowngradeFingerprint{Request: request, Selection: request.Selection})
	if err != nil {
		t.Fatalf("marshal fingerprint: %v", err)
	}
	return string(payload)
}

func assistantDowngradeFunctionParts(t *testing.T, contents []Content) [][]Part {
	t.Helper()
	parts := make([][]Part, len(contents))
	for index, content := range contents {
		parts[index] = content.Parts
	}
	return parts
}

func TestAssistantTextDowngradeNonAssistantUntouched(t *testing.T) {
	for _, channel := range []Channel{"", ChannelPlayground, ChannelBuild} {
		t.Run("channel="+string(channel), func(t *testing.T) {
			request := assistantDowngradeRequest()
			request.Selection.Channel = channel
			request.Selection.AntiTruncation = true
			parallel := true
			request.Tools = Tools{
				Functions:    []FunctionDeclaration{{Name: "read_fixture"}},
				Google:       []string{"google_search"},
				GoogleSearch: &GoogleSearchOptions{WebSearch: true},
				ToolConfig:   ToolConfig{Mode: "required", AllowedFunctionNames: []string{"read_fixture"}, ParallelCalls: &parallel},
			}
			request.Contents = append(request.Contents, Content{
				Role:  RoleAssistant,
				Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "read_fixture", Arguments: json.RawMessage(`{}`)}}},
			})
			before := assistantDowngradeFingerprintOf(t, request)
			prepared, err := prepareAssistantTextRequest(request)
			if err != nil {
				t.Fatalf("non-assistant request must pass through untouched: %v", err)
			}
			if !reflect.DeepEqual(prepared, request) {
				t.Fatalf("non-assistant request was modified:\n got: %+v\nwant: %+v", prepared, request)
			}
			if after := assistantDowngradeFingerprintOf(t, prepared); after != before {
				t.Fatalf("non-assistant request payload changed:\n got: %s\nwant: %s", after, before)
			}
		})
	}
}

func TestAssistantTextDowngradeRejectsAntiTruncation(t *testing.T) {
	for _, withTools := range []bool{false, true} {
		t.Run(map[bool]string{false: "text-only", true: "with-tools"}[withTools], func(t *testing.T) {
			request := assistantDowngradeRequest()
			request.Selection.AntiTruncation = true
			if withTools {
				request.Tools.Functions = []FunctionDeclaration{{Name: "read_fixture"}}
			}
			prepared, err := prepareAssistantTextRequest(request)
			if err == nil {
				t.Fatal("assistant anti-truncation must be refused, not downgraded")
			}
			if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "anti-truncation") {
				t.Fatalf("anti-truncation refusal lost its boundary: %v", err)
			}
			if !reflect.DeepEqual(prepared, GenerateRequest{}) {
				t.Fatalf("refused request returned a partial request: %+v", prepared)
			}
		})
	}
}

func TestAssistantTextDowngradeClearsAllToolContracts(t *testing.T) {
	parallel := true
	for _, mode := range []string{"", "none", "auto", "required", "validated"} {
		t.Run("mode="+mode, func(t *testing.T) {
			request := assistantDowngradeRequest()
			request.Tools = Tools{
				Functions:    []FunctionDeclaration{{Name: "read_fixture", Parameters: json.RawMessage(`{"type":"object"}`)}},
				Google:       []string{"google_search"},
				GoogleSearch: &GoogleSearchOptions{WebSearch: true},
				ToolConfig:   ToolConfig{Mode: mode, AllowedFunctionNames: []string{"read_fixture"}, ParallelCalls: &parallel},
			}
			prepared, err := prepareAssistantTextRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(prepared.Tools, Tools{}) {
				t.Fatalf("assistant wire must carry no tools for mode %q: %+v", mode, prepared.Tools)
			}
		})
	}
}

// 没有声明、只有 required / parallel 约束时也必须清理：Assistant 文本策略无法
// 保证 required 语义，也不能假装 parallel 生效。
func TestAssistantTextDowngradeClearsChoiceWithoutDeclarations(t *testing.T) {
	parallel := false
	request := assistantDowngradeRequest()
	request.Tools.ToolConfig = ToolConfig{Mode: "required", ParallelCalls: &parallel}
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Tools, Tools{}) {
		t.Fatalf("tool choice survived without declarations: %+v", prepared.Tools)
	}
}

func TestAssistantTextDowngradeGoogleTools(t *testing.T) {
	known := []string{"code_execution", "google_search", "image_search", "google_maps", "url_context"}
	t.Run("known", func(t *testing.T) {
		request := assistantDowngradeRequest()
		request.Tools.Google = append([]string(nil), known...)
		prepared, err := prepareAssistantTextRequest(request)
		if err != nil {
			t.Fatalf("known Google tools must be filtered, not refused: %v", err)
		}
		if !reflect.DeepEqual(prepared.Tools, Tools{}) {
			t.Fatalf("known Google tools survived: %+v", prepared.Tools)
		}
	})
	t.Run("mixed", func(t *testing.T) {
		request := assistantDowngradeRequest()
		request.Tools = Tools{
			Functions:    []FunctionDeclaration{{Name: "read_fixture"}},
			Google:       []string{"google_search", "url_context"},
			GoogleSearch: &GoogleSearchOptions{WebSearch: true},
			ToolConfig:   ToolConfig{Mode: "auto"},
		}
		prepared, err := prepareAssistantTextRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(prepared.Tools, Tools{}) {
			t.Fatalf("mixed tool contracts survived: %+v", prepared.Tools)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		for _, mode := range []string{"", "none"} {
			request := assistantDowngradeRequest()
			request.Tools = Tools{Google: []string{"google_search", "bogus_tool"}, ToolConfig: ToolConfig{Mode: mode}}
			if _, err := prepareAssistantTextRequest(request); err == nil {
				t.Fatalf("unknown Google tool passed with mode %q", mode)
			} else if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "未知 Google tool") {
				t.Fatalf("unknown Google tool lost its upstream rule: %v", err)
			}
		}
	})
}

func TestAssistantTextDowngradeTranscribesFunctionHistory(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
		{Role: RoleAssistant, Parts: []Part{{
			FunctionCall:     &FunctionCall{ID: "call-1", Name: "read_fixture", Arguments: json.RawMessage(`{"path":"a"}`), ThoughtSignature: "opaque-signature"},
			ThoughtSignature: "part-signature",
			Thought:          true,
		}}},
		// 缺 name 的结果必须按调用 ID 关联到 read_fixture。
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Content: json.RawMessage(`{"value":1}`)}}}},
	}
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parts := assistantDowngradeFunctionParts(t, prepared.Contents)
	if got, want := parts[0][0].Text, "fixture question"; got != want {
		t.Fatalf("plain text part changed: got %q want %q", got, want)
	}
	if got, want := parts[1][0].Text, `{"functionCall":{"args":{"path":"a"},"name":"read_fixture"}}`; got != want {
		t.Fatalf("function call transcript: got %q want %q", got, want)
	}
	if got, want := parts[2][0].Text, `{"functionResponse":{"name":"read_fixture","response":{"value":1}}}`; got != want {
		t.Fatalf("function response transcript: got %q want %q", got, want)
	}
	for contentIndex, content := range prepared.Contents {
		for partIndex, part := range content.Parts {
			if part.FunctionCall != nil || part.FunctionResult != nil {
				t.Fatalf("contents[%d].parts[%d] still carries native function history", contentIndex, partIndex)
			}
			if part.ThoughtSignature != "" || part.Thought {
				t.Fatalf("contents[%d].parts[%d] leaked signature/thought state into text", contentIndex, partIndex)
			}
		}
	}
	if !reflect.DeepEqual(prepared.Tools, Tools{}) {
		t.Fatalf("tools survived history transcription: %+v", prepared.Tools)
	}
}

// linkFunctionResults 在补齐缺失名称时会克隆 Parts。把 linked.Parts 作为唯一
// 工作切片后，同一 content 里紧随其后的普通 Part 仍必须原样保留，不能因为
// 写回旧切片而丢失补齐或误改后续内容。
func TestAssistantTextDowngradeBackfillsNameWithinOneContent(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "read_fixture", Arguments: json.RawMessage(`{}`)}}}},
		{Role: RoleTool, Parts: []Part{
			{FunctionResult: &FunctionResult{ID: "call-1", Content: json.RawMessage(`{"ok":1}`)}},
			{Text: "trailing text", ThoughtSignature: "keep"},
		}},
	}
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parts := assistantDowngradeFunctionParts(t, prepared.Contents)
	if got, want := parts[1][0].Text, `{"functionCall":{"args":{},"name":"read_fixture"}}`; got != want {
		t.Fatalf("function call transcript: got %q want %q", got, want)
	}
	if got, want := parts[2][0].Text, `{"functionResponse":{"name":"read_fixture","response":{"ok":1}}}`; got != want {
		t.Fatalf("backfilled function response transcript: got %q want %q", got, want)
	}
	if got := parts[2][1]; got.Text != "trailing text" || got.ThoughtSignature != "keep" {
		t.Fatalf("sibling part after the cloned function result changed: %+v", got)
	}
}

func TestAssistantTextDowngradeLinksResultByCallID(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "two calls"}}},
		{Role: RoleAssistant, Parts: []Part{
			{FunctionCall: &FunctionCall{ID: "call-alpha", Name: "alpha", Arguments: json.RawMessage(`{}`)}},
			{FunctionCall: &FunctionCall{ID: "call-beta", Name: "beta", Arguments: json.RawMessage(`{}`)}},
		}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-beta", Content: json.RawMessage(`{"ok":true}`)}}}},
	}
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parts := assistantDowngradeFunctionParts(t, prepared.Contents)
	if got, want := parts[0][0].Text, "two calls"; got != want {
		t.Fatalf("plain text part changed: got %q want %q", got, want)
	}
	if got, want := parts[1][0].Text, `{"functionCall":{"args":{},"name":"alpha"}}`; got != want {
		t.Fatalf("alpha transcript: got %q want %q", got, want)
	}
	if got, want := parts[1][1].Text, `{"functionCall":{"args":{},"name":"beta"}}`; got != want {
		t.Fatalf("beta transcript: got %q want %q", got, want)
	}
	if got, want := parts[2][0].Text, `{"functionResponse":{"name":"beta","response":{"ok":true}}}`; got != want {
		t.Fatalf("ID-linked response: got %q want %q", got, want)
	}
}

func TestAssistantTextDowngradeRejectsUnlinkableResultName(t *testing.T) {
	t.Run("no-pending-call", func(t *testing.T) {
		request := assistantDowngradeRequest()
		request.Contents = []Content{
			{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
			{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{Content: json.RawMessage(`{}`)}}}},
		}
		if _, err := prepareAssistantTextRequest(request); err == nil {
			t.Fatal("nameless result without a pending call was silently accepted")
		} else if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "缺少名称") {
			t.Fatalf("unlinkable result lost its evidence: %v", err)
		}
	})
	t.Run("ambiguous-pending-calls", func(t *testing.T) {
		request := assistantDowngradeRequest()
		request.Contents = []Content{
			{Role: RoleUser, Parts: []Part{{Text: "two calls"}}},
			{Role: RoleAssistant, Parts: []Part{
				{FunctionCall: &FunctionCall{ID: "call-alpha", Name: "alpha", Arguments: json.RawMessage(`{}`)}},
				{FunctionCall: &FunctionCall{ID: "call-beta", Name: "beta", Arguments: json.RawMessage(`{}`)}},
			}},
			{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{Content: json.RawMessage(`{}`)}}}},
		}
		if _, err := prepareAssistantTextRequest(request); err == nil {
			t.Fatal("ambiguous nameless result was silently accepted")
		} else if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "缺少名称") {
			t.Fatalf("ambiguous result lost its evidence: %v", err)
		}
	})
}

func TestAssistantTextDowngradeEmptyArgumentsBecomeEmptyObject(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "noop"}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Name: "noop"}}}},
	}
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parts := assistantDowngradeFunctionParts(t, prepared.Contents)
	if got, want := parts[0][0].Text, "fixture question"; got != want {
		t.Fatalf("plain text part changed: got %q want %q", got, want)
	}
	if got, want := parts[1][0].Text, `{"functionCall":{"args":{},"name":"noop"}}`; got != want {
		t.Fatalf("empty call args: got %q want %q", got, want)
	}
	if got, want := parts[2][0].Text, `{"functionResponse":{"name":"noop","response":{}}}`; got != want {
		t.Fatalf("empty response body: got %q want %q", got, want)
	}
}

func TestAssistantTextDowngradeRejectsInvalidJSON(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "noop", Arguments: json.RawMessage(`{`)}}}},
	}
	if _, err := prepareAssistantTextRequest(request); err == nil {
		t.Fatal("invalid function arguments were silently accepted")
	} else if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid JSON lost its invalid-argument boundary: %v", err)
	}
}

// code 历史不在本模块转录：保留原 Part 供后续 validator 拒绝，不扩大能力。
func TestAssistantTextDowngradeKeepsCodeHistoryForValidator(t *testing.T) {
	code := &ExecutableCode{Language: "PYTHON", Code: "print(1)"}
	result := &CodeExecutionResult{Outcome: "OUTCOME_OK", Output: "1"}
	request := assistantDowngradeRequest()
	request.Contents = append(request.Contents, Content{Role: RoleAssistant, Parts: []Part{
		{ExecutableCode: code},
		{CodeExecutionResult: result},
	}})
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parts := assistantDowngradeFunctionParts(t, prepared.Contents)
	if parts[1][0].ExecutableCode != code || parts[1][1].CodeExecutionResult != result {
		t.Fatalf("code history was transcribed or dropped instead of being left for the validator: %+v", parts[1])
	}
}

// 普通文本 Part（包括思考标记与签名）不能因为降级被改写。
func TestAssistantTextDowngradeKeepsPlainTextParts(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Contents = append(request.Contents, Content{Role: RoleAssistant, Parts: []Part{
		{Text: "visible", Thought: true, ThoughtSignature: "opaque"},
	}})
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Contents[1].Parts[0], request.Contents[1].Parts[0]) {
		t.Fatalf("plain text part changed: got %+v want %+v", prepared.Contents[1].Parts[0], request.Contents[1].Parts[0])
	}
}

func TestAssistantTextDowngradeNilAndEmptyContents(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		request := assistantDowngradeRequest()
		request.Contents = nil
		prepared, err := prepareAssistantTextRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Contents != nil {
			t.Fatalf("nil contents became %#v", prepared.Contents)
		}
	})
	t.Run("empty", func(t *testing.T) {
		request := assistantDowngradeRequest()
		request.Contents = []Content{}
		prepared, err := prepareAssistantTextRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Contents == nil || len(prepared.Contents) != 0 {
			t.Fatalf("empty contents changed: %#v", prepared.Contents)
		}
	})
}

// 预处理不能原地改动调用方输入，包括缺 name 结果补齐名称的路径。
func TestAssistantTextDowngradeDoesNotMutateInput(t *testing.T) {
	parallel := true
	request := assistantDowngradeRequest()
	request.Tools = Tools{
		Functions:  []FunctionDeclaration{{Name: "read_fixture", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolConfig: ToolConfig{Mode: "required", ParallelCalls: &parallel},
	}
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "read_fixture", Arguments: json.RawMessage(`{"path":"a"}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Content: json.RawMessage(`{"value":1}`)}}}},
	}
	before := assistantDowngradeFingerprintOf(t, request)
	if _, err := prepareAssistantTextRequest(request); err != nil {
		t.Fatal(err)
	}
	if after := assistantDowngradeFingerprintOf(t, request); after != before {
		t.Fatalf("input request was mutated:\n got: %s\nwant: %s", after, before)
	}
	// 原有函数历史必须仍可被调用方复用（名称没有被补写、Part 没有被替换）。
	if request.Contents[2].Parts[0].FunctionResult.Name != "" {
		t.Fatalf("input function result name was backfilled in place: %+v", request.Contents[2].Parts[0])
	}
	if request.Contents[1].Parts[0].FunctionCall == nil {
		t.Fatal("input function call part was replaced in place")
	}
}

func TestAssistantTextDowngradeIdempotent(t *testing.T) {
	request := assistantDowngradeRequest()
	request.Tools = Tools{
		Functions:    []FunctionDeclaration{{Name: "read_fixture"}},
		Google:       []string{"google_search"},
		GoogleSearch: &GoogleSearchOptions{WebSearch: true},
		ToolConfig:   ToolConfig{Mode: "auto", AllowedFunctionNames: []string{"read_fixture"}},
	}
	request.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-1", Name: "read_fixture", Arguments: json.RawMessage(`{"path":"a"}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-1", Content: json.RawMessage(`{"value":1}`)}}}},
	}
	first, err := prepareAssistantTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareAssistantTextRequest(first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("normalization is not idempotent:\n first: %+v\nsecond: %+v", first, second)
	}
}

// 两个不同请求并发规范化时不能共享任何状态（例如待返回调用链）。
func TestAssistantTextDowngradeConcurrentRequestsDoNotShareState(t *testing.T) {
	requestA := assistantDowngradeRequest()
	requestA.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "request A"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-a", Name: "alpha", Arguments: json.RawMessage(`{}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-a", Content: json.RawMessage(`{"a":1}`)}}}},
	}
	requestB := assistantDowngradeRequest()
	requestB.Contents = []Content{
		{Role: RoleUser, Parts: []Part{{Text: "request B"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "call-b", Name: "beta", Arguments: json.RawMessage(`{}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "call-b", Content: json.RawMessage(`{"b":2}`)}}}},
	}
	expectedA, err := prepareAssistantTextRequest(requestA)
	if err != nil {
		t.Fatal(err)
	}
	expectedB, err := prepareAssistantTextRequest(requestB)
	if err != nil {
		t.Fatal(err)
	}
	failures := make(chan string, 32)
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			prepared, err := prepareAssistantTextRequest(requestA)
			if err != nil {
				failures <- "request A: " + err.Error()
				return
			}
			if !reflect.DeepEqual(prepared, expectedA) {
				failures <- "request A: got a cross-request result"
			}
		}()
		go func() {
			defer wait.Done()
			prepared, err := prepareAssistantTextRequest(requestB)
			if err != nil {
				failures <- "request B: " + err.Error()
				return
			}
			if !reflect.DeepEqual(prepared, expectedB) {
				failures <- "request B: got a cross-request result"
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}
