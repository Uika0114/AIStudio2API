package aistudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These fixtures stop below the protected transport. They never create a WAA
// worker, browser, listener, credentials or external HTTP client. Every
// "upstream" byte in this file is synthetic.

type assistantAuxTransport struct {
	fixture *assistantFixture
}

func (transport assistantAuxTransport) Do(ctx context.Context, rpc RPCRequest) (*RPCResponse, error) {
	fixture := transport.fixture
	fixture.mu.Lock()
	fixture.auxSends = append(fixture.auxSends, rpc)
	streamBody := append([]byte(nil), fixture.streamBody...)
	streamErr := fixture.streamErr
	fixture.mu.Unlock()
	switch rpc.Method {
	case assistantCreateAppletMethod:
		return fixture.jsonResponse([]byte(`["APPLET-1",["APPLET-1"]]`)), nil
	case assistantStreamMethod:
		if streamErr != nil {
			return nil, streamErr
		}
		return fixture.jsonResponse(streamBody), nil
	case assistantCancelMethod, assistantDeleteAppletMethod:
		if lease, ok := AccountLeaseFromContext(ctx); ok {
			fixture.mu.Lock()
			fixture.cleanupLease = lease
			fixture.mu.Unlock()
		}
		fixture.mu.Lock()
		fixture.cleanupCtxErr = ctx.Err()
		fail := fixture.cleanupFail
		fixture.mu.Unlock()
		fixture.signalCleanup(rpc.Method)
		if fail {
			return &RPCResponse{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader("fixture cleanup failure"))}, nil
		}
		return fixture.jsonResponse([]byte(`[]`)), nil
	default:
		return nil, fmt.Errorf("fixture: unexpected auxiliary RPC %s", rpc.Method)
	}
}

type assistantProtectedTransport struct {
	fixture *assistantFixture
}

func (transport assistantProtectedTransport) DoProtected(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	fixture := transport.fixture
	if err := request.Selection.ValidateChannel(generationRPCChannel(rpc.Method)); err != nil {
		return nil, err
	}
	ctx, attempt := beginPhysicalAttempt(ctx, request, rpc)
	markPhysicalSend(ctx)
	fixture.mu.Lock()
	fixture.protectedSends = append(fixture.protectedSends, rpc)
	initBody := append([]byte(nil), fixture.initBody...)
	fixture.mu.Unlock()
	return attempt.attach(fixture.jsonResponse(initBody)), nil
}

type assistantFixture struct {
	client         *Client
	ctx            context.Context
	mu             sync.Mutex
	auxSends       []RPCRequest
	protectedSends []RPCRequest
	attempts       map[string]GenerationAttempt
	initBody       []byte
	streamBody     []byte
	streamErr      error
	cleanupCalls   []string
	cleanupDone    chan string
	cleanupLease   *AccountLease
	cleanupCtxErr  error
	cleanupFail    bool
	progress       atomic.Bool
}

func newAssistantFixture(t *testing.T) *assistantFixture {
	t.Helper()
	fixture := &assistantFixture{
		attempts:    map[string]GenerationAttempt{},
		initBody:    []byte(`["GEN-1","APPLET-1"]`),
		cleanupDone: make(chan string, 8),
	}
	fixture.ctx = ContextWithGenerationProgressObserver(ContextWithGenerationAttemptObserver(context.Background(), func(snapshot GenerationAttempt) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if previous, ok := fixture.attempts[snapshot.ID]; !ok || snapshot.Revision >= previous.Revision {
			fixture.attempts[snapshot.ID] = snapshot
		}
	}), func() { fixture.progress.Store(true) })
	client, err := NewClient(ClientOptions{
		Transport: assistantAuxTransport{fixture: fixture},
		Protected: assistantProtectedTransport{fixture: fixture},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.client = client
	return fixture
}

func (fixture *assistantFixture) jsonResponse(body []byte) *RPCResponse {
	return &RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func (fixture *assistantFixture) signalCleanup(method string) {
	fixture.mu.Lock()
	fixture.cleanupCalls = append(fixture.cleanupCalls, method)
	fixture.mu.Unlock()
	fixture.cleanupDone <- method
}

func (fixture *assistantFixture) waitCleanup(t *testing.T, method string) {
	t.Helper()
	select {
	case got := <-fixture.cleanupDone:
		if got != method {
			t.Fatalf("cleanup order: got %s, want %s", got, method)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("cleanup %s was not called", method)
	}
}

func (fixture *assistantFixture) snapshot() (aux []RPCRequest, protected []RPCRequest, attempts map[string]GenerationAttempt) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	aux = append([]RPCRequest(nil), fixture.auxSends...)
	protected = append([]RPCRequest(nil), fixture.protectedSends...)
	attempts = make(map[string]GenerationAttempt, len(fixture.attempts))
	for id, attempt := range fixture.attempts {
		attempts[id] = attempt
	}
	return aux, protected, attempts
}

func assistantTestRequest() GenerateRequest {
	return GenerateRequest{
		ID:        "req-1",
		Model:     "gemini-4-argon",
		AccountID: "fixture-account",
		Selection: ModelSelection{
			RequestedModel: "assistant/gemini-4-argon",
			BaseModel:      "gemini-4-argon",
			Channel:        ChannelAssistant,
		},
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "What is 6*7?"}}}},
	}
}

func assistantTestEntry() modelEntry {
	return modelEntry{model: Model{ID: "gemini-4-argon"}, defaults: GenerationDefaults{}}
}

// assistantPart builds one encoded Part: field 2 text, field 13 thought.
func assistantPart(text string, thought bool) []any {
	part := make([]any, 13)
	part[1] = text
	if thought {
		part[12] = true
	}
	return part
}

// assistantStep builds one encoded step: field 1 contents, 6 failure, 12 number,
// 13 status, 16 usage.
func assistantStep(role string, number int64, status int64, usage []any, parts ...[]any) []any {
	partList := make([]any, len(parts))
	for index, part := range parts {
		partList[index] = part
	}
	step := make([]any, 17)
	step[0] = []any{[]any{partList, role}}
	step[11] = number
	step[12] = status
	if usage != nil {
		step[15] = usage
	}
	return step
}

func assistantMessage(steps ...[]any) []any {
	list := make([]any, len(steps))
	for index, step := range steps {
		list[index] = step
	}
	return []any{nil, nil, list, "APPLET-1"}
}

func assistantStreamBytes(messages ...[]any) []byte {
	list := make([]any, len(messages))
	for index, message := range messages {
		list[index] = message
	}
	body, err := json.Marshal([]any{list})
	if err != nil {
		panic(err)
	}
	return body
}

func assistantCollect(t *testing.T, source io.Reader) ([]Event, error) {
	t.Helper()
	var events []Event
	err := DecodeAssistantStream(source, &AssistantStreamDecoder{}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func assistantText(events []Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Kind == EventText {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func assistantReasoning(events []Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Kind == EventReasoning {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func assistantFinish(events []Event) *Event {
	for index := range events {
		if events[index].Kind == EventFinish {
			return &events[index]
		}
	}
	return nil
}

func assistantUsage(events []Event) *Usage {
	for _, event := range events {
		if event.Kind == EventUsage {
			return event.Usage
		}
	}
	return nil
}

func TestAssistantEncodeRequestUsesBaseModelAndThinking(t *testing.T) {
	request := assistantTestRequest()
	request.System = "be brief"
	request.Contents = []Content{{Role: RoleUser, Parts: []Part{{Text: "What is 6*7?"}, {Text: "Answer briefly."}}}}
	request.Config = GenerationConfig{ReasoningEffort: "low"}
	body, err := EncodeAssistantRequest(request, GenerationDefaults{}, "APPLET-1", "REQ-ID")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("assistant/")) {
		t.Fatalf("local routing prefix leaked onto the wire: %s", body)
	}
	var wire []json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 29 {
		t.Fatalf("field count: %d", len(wire))
	}
	if string(wire[7]) != `"models/gemini-4-argon"` {
		t.Fatalf("wire model: %s", wire[7])
	}
	if string(wire[8]) != `"be brief"` {
		t.Fatalf("system: %s", wire[8])
	}
	if string(wire[11]) != `"APPLET-1"` || string(wire[28]) != `"REQ-ID"` {
		t.Fatalf("applet/request id: %s %s", wire[11], wire[28])
	}
	if !isJSONNull(wire[1]) {
		t.Fatal("the encoder wrote a WAA proof; only the protected transport may")
	}
	var thinking []json.RawMessage
	if err := json.Unmarshal(wire[18], &thinking); err != nil {
		t.Fatal(err)
	}
	if len(thinking) != 4 || string(thinking[3]) != "1" {
		t.Fatalf("thinking config: %s", wire[18])
	}
	prompt, err := assistantBindingPrompt(body)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "What is 6*7? Answer briefly." {
		t.Fatalf("binding: %q", prompt)
	}
}

func TestAssistantThinkingLevelSelection(t *testing.T) {
	defaults := GenerationDefaults{ThinkingLevels: []int64{1, 2, 3}}
	for _, test := range []struct {
		config GenerationConfig
		want   int64
	}{
		{GenerationConfig{}, 3},
		{GenerationConfig{ReasoningEffort: "low"}, 1},
		{GenerationConfig{ReasoningEffort: "medium"}, 2},
		{GenerationConfig{ReasoningEffort: "high"}, 3},
		{GenerationConfig{ReasoningEffort: "max"}, 3},
		{GenerationConfig{ReasoningEffort: "none"}, 1},
		{GenerationConfig{ThinkingBudget: int64Ptr(0)}, 1},
		{GenerationConfig{ThinkingBudget: int64Ptr(4096)}, 2},
		{GenerationConfig{ThinkingBudget: int64Ptr(65536)}, 3},
	} {
		level, err := assistantThinkingLevel(test.config, defaults)
		if err != nil || level != test.want {
			t.Fatalf("config %+v: level %d err %v, want %d", test.config, level, err, test.want)
		}
	}
	if _, err := assistantThinkingLevel(GenerationConfig{ReasoningEffort: "extreme"}, defaults); err == nil {
		t.Fatal("unknown reasoning effort accepted")
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestAssistantBindingPromptIncludesInlineBase64(t *testing.T) {
	contents, err := encodeContents([]Content{{Role: RoleUser, Parts: []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte("abc")}}}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]any{contents})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := assistantBindingPrompt(body)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != base64.StdEncoding.EncodeToString([]byte("abc")) {
		t.Fatalf("binding: %q", prompt)
	}
}

func TestAssistantValidateFailsClosed(t *testing.T) {
	cases := map[string]func(*GenerateRequest){
		"functions":       func(request *GenerateRequest) { request.Tools.Functions = []FunctionDeclaration{{Name: "real"}} },
		"google":          func(request *GenerateRequest) { request.Tools.Google = []string{"google_search"} },
		"google_search":   func(request *GenerateRequest) { request.Tools.GoogleSearch = &GoogleSearchOptions{WebSearch: true} },
		"required_choice": func(request *GenerateRequest) { request.Tools.ToolConfig.Mode = "required" },
		"allowed_names":   func(request *GenerateRequest) { request.Tools.ToolConfig.AllowedFunctionNames = []string{"real"} },
		"function_call": func(request *GenerateRequest) {
			request.Contents[0].Parts[0].FunctionCall = &FunctionCall{Name: "real", Arguments: json.RawMessage(`{}`)}
		},
		"function_result": func(request *GenerateRequest) {
			request.Contents[0].Parts[0].FunctionResult = &FunctionResult{Name: "real", Content: json.RawMessage(`{}`)}
		},
		"code_execution": func(request *GenerateRequest) {
			request.Contents[0].Parts[0].ExecutableCode = &ExecutableCode{Language: "PYTHON", Code: "1"}
		},
		"anti_truncation": func(request *GenerateRequest) { request.Selection.AntiTruncation = true },
		"inline_data": func(request *GenerateRequest) {
			request.Contents[0].Parts[0].InlineData = &Blob{MIME: "image/png", Data: []byte("png")}
		},
		"external_media": func(request *GenerateRequest) {
			request.Contents[0].Parts[0].ExternalMedia = &ExternalMedia{MIME: "image/png", URL: "https://example.invalid/x"}
		},
		"drive_file":        func(request *GenerateRequest) { request.Contents[0].Parts[0].File = &FileRef{ID: "file-1"} },
		"max_output_tokens": func(request *GenerateRequest) { request.Config.MaxOutputTokens = int64Ptr(1024) },
		"temperature":       func(request *GenerateRequest) { value := 0.2; request.Config.Temperature = &value },
		"response_schema":   func(request *GenerateRequest) { request.Config.ResponseSchema = json.RawMessage(`{"type":"object"}`) },
		"response_modalities": func(request *GenerateRequest) {
			request.Config.ResponseModalities = []ResponseModality{ResponseModalityImage}
		},
		"safety_settings": func(request *GenerateRequest) {
			request.SafetySettings = []SafetySetting{{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_NONE"}}
		},
		"unknown_effort": func(request *GenerateRequest) {
			request.Config.ReasoningEffort = "extreme"
		},
	}
	if assistantFunctionToolsImplemented() {
		t.Fatal("fixture expects no verified client-tool mapping")
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := assistantTestRequest()
			mutate(&request)
			err := validateAssistantRequest(request)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("not invalid argument: %v", err)
			}
			if !strings.Contains(err.Error(), "assistant") {
				t.Fatalf("error does not identify the assistant adapter: %v", err)
			}
		})
	}
	if err := validateAssistantRequest(assistantTestRequest()); err != nil {
		t.Fatalf("plain text request rejected: %v", err)
	}
}

func TestAssistantAntiRequestFailsClosedBeforeAnyUpstreamSend(t *testing.T) {
	fixture := newAssistantFixture(t)
	request := assistantTestRequest()
	request.Selection.AntiTruncation = true
	_, _, err := fixture.client.sendAssistant(fixture.ctx, request, assistantTestEntry())
	if err == nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("tool request was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "assistant tool protocol mapping not implemented") {
		t.Fatalf("error must name the unimplemented mapping, not upstream incapability: %v", err)
	}
	aux, protected, attempts := fixture.snapshot()
	if len(aux) != 0 || len(protected) != 0 || len(attempts) != 0 {
		t.Fatalf("fail-closed request touched upstream: aux=%d protected=%d attempts=%d", len(aux), len(protected), len(attempts))
	}
}

func TestAssistantStreamDecodesSnapshotDeltasUsageAndFinish(t *testing.T) {
	stream := assistantStreamBytes(assistantMessage(
		assistantStep("user", 0, 1, nil, assistantPart("What is 6*7?", false)),
		assistantStep("model", 1, 2, nil, assistantPart("The answer", false), assistantPart("Let me think", true)),
		assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("The answer is 42", false), assistantPart("Let me think", true)),
	))
	events, err := assistantCollect(t, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if text := assistantText(events); text != "The answer is 42" {
		t.Fatalf("text: %q", text)
	}
	if reasoning := assistantReasoning(events); reasoning != "Let me think" {
		t.Fatalf("reasoning: %q", reasoning)
	}
	usage := assistantUsage(events)
	if usage == nil || usage.InputTokens != 12 || usage.CachedTokens != 2 || usage.OutputTokens != 5 || usage.TotalTokens != 17 {
		t.Fatalf("usage: %+v", usage)
	}
	finish := assistantFinish(events)
	if finish == nil || finish.FinishReason != "stop" {
		t.Fatalf("finish: %+v", finish)
	}
}

func TestAssistantStreamSeparatesSteps(t *testing.T) {
	stream := assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 1, nil, assistantPart("First", false)),
		assistantStep("model", 2, 1, nil, assistantPart("Second", false)),
	))
	events, err := assistantCollect(t, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if text := assistantText(events); text != "First\n\nSecond" {
		t.Fatalf("text: %q", text)
	}
}

func TestAssistantStreamRejectsNonContinuation(t *testing.T) {
	stream := assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 2, nil, assistantPart("abcdef", false)),
		assistantStep("model", 1, 1, nil, assistantPart("xyz", false)),
	))
	_, err := assistantCollect(t, bytes.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), "步骤正文不是上一次快照的延续") {
		t.Fatalf("expected snapshot violation, got %v", err)
	}
}

func TestAssistantStreamMissingFinishNeverBecomesSuccess(t *testing.T) {
	stream := assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 2, nil, assistantPart("partial", false)),
	))
	events, err := assistantCollect(t, bytes.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), "流结束前没有完成的模型回复") {
		t.Fatalf("missing finish accepted: %v", err)
	}
	if assistantFinish(events) != nil {
		t.Fatal("missing finish synthesized a terminal event")
	}
}

func TestAssistantStreamTruncatedEnvelopeFails(t *testing.T) {
	stream := assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 1, nil, assistantPart("done", false)),
	))
	_, err := assistantCollect(t, bytes.NewReader(stream[:len(stream)-1]))
	if err == nil {
		t.Fatal("truncated stream accepted")
	}
}

func TestAssistantStreamStepFailureAndTrailer(t *testing.T) {
	failed := assistantStep("model", 1, 4, nil, assistantPart("x", false))
	failed[5] = []any{"model overloaded"}
	stream := assistantStreamBytes(assistantMessage(failed))
	_, err := assistantCollect(t, bytes.NewReader(stream))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.StatusCode != http.StatusBadGateway || !strings.Contains(rpcErr.Message, "生成步骤失败") {
		t.Fatalf("step failure: %v", err)
	}

	trailer := []byte(`[[], [8, "quota exhausted"]]`)
	_, err = assistantCollect(t, bytes.NewReader(trailer))
	if !errors.As(err, &rpcErr) || rpcErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("trailer error: %v", err)
	}
}

func TestAssistantSendPlainTextLifecycleAndAudit(t *testing.T) {
	fixture := newAssistantFixture(t)
	fixture.streamBody = assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("Answer", false)),
	))
	request := assistantTestRequest()
	response, decode, err := fixture.client.sendAssistant(fixture.ctx, request, assistantTestEntry())
	if err != nil {
		t.Fatal(err)
	}
	aux, protected, _ := fixture.snapshot()
	if len(protected) != 1 || protected[0].Method != assistantInitMethod {
		t.Fatalf("protected sends: %+v", protected)
	}
	if protected[0].Method != "CodeAssistantOffline" {
		t.Fatalf("the protected generation send must be CodeAssistantOffline, got %s", protected[0].Method)
	}
	if len(aux) != 2 || aux[0].Method != assistantCreateAppletMethod || aux[1].Method != assistantStreamMethod {
		t.Fatalf("auxiliary sends: %+v", aux)
	}
	if aux[1].Method != "StreamCodeAssistantOfflineGeneration" {
		t.Fatalf("stream method: %s", aux[1].Method)
	}
	var streamWire []json.RawMessage
	if err := json.Unmarshal(aux[1].Body, &streamWire); err != nil {
		t.Fatal(err)
	}
	if string(streamWire[0]) != `"GEN-1"` || string(streamWire[3]) != `"APPLET-1"` {
		t.Fatalf("stream request body: %s", aux[1].Body)
	}

	var events []Event
	if err := decode(response.Body, func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if assistantText(events) != "Answer" || assistantFinish(events) == nil {
		t.Fatalf("projection: %+v", events)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.waitCleanup(t, assistantDeleteAppletMethod)

	_, protected, attempts := fixture.snapshot()
	if len(protected) != 1 {
		t.Fatalf("stream retried the generation submit: %d", len(protected))
	}
	if len(attempts) != 1 {
		t.Fatalf("expected exactly one generation attempt, got %d", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Method != "CodeAssistantOffline" || attempt.Channel != ChannelAssistant || attempt.RequestedChannel != ChannelAssistant {
			t.Fatalf("attempt identity: %+v", attempt)
		}
		if !attempt.Sent || !attempt.ResponseReceived {
			t.Fatalf("attempt not audited as sent: %+v", attempt)
		}
		if attempt.Usage == nil || attempt.Usage.TotalTokens != 17 || attempt.UsageSource != "upstream" {
			t.Fatalf("attempt usage: %+v", attempt.Usage)
		}
		if attempt.Stage != "upstream_finished" {
			t.Fatalf("attempt stage: %s", attempt.Stage)
		}
		if attempt.RequestedModel != "assistant/gemini-4-argon" || attempt.BaseModel != "gemini-4-argon" {
			t.Fatalf("attempt model identity: %+v", attempt)
		}
	}
	fixture.mu.Lock()
	calls := append([]string(nil), fixture.cleanupCalls...)
	fixture.mu.Unlock()
	if len(calls) != 1 || calls[0] != assistantDeleteAppletMethod {
		t.Fatalf("completed stream must not be cancelled: %v", calls)
	}
}

func TestAssistantSendFailureCancelsThenDeletes(t *testing.T) {
	fixture := newAssistantFixture(t)
	fixture.streamBody = assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 2, nil, assistantPart("partial", false)),
	))
	response, decode, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry())
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(response.Body, func(Event) error { return nil }); err == nil {
		t.Fatal("unfinished assistant stream decoded as success")
	}
	_ = response.Body.Close()
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)

	aux, _, _ := fixture.snapshot()
	cancelBody := ""
	for _, send := range aux {
		if send.Method == assistantCancelMethod {
			cancelBody = string(send.Body)
		}
	}
	if cancelBody != `["GEN-1","APPLET-1"]` {
		t.Fatalf("cancel body: %s", cancelBody)
	}
}

func TestAssistantInitFailureDeletesAppletWithoutStream(t *testing.T) {
	fixture := newAssistantFixture(t)
	fixture.initBody = []byte(`[]`)
	_, _, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry())
	if err == nil || !strings.Contains(err.Error(), "缺少任务 ID") {
		t.Fatalf("missing task ID: %v", err)
	}
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
	aux, protected, _ := fixture.snapshot()
	if len(protected) != 1 {
		t.Fatalf("protected sends: %d", len(protected))
	}
	for _, send := range aux {
		if send.Method == assistantStreamMethod {
			t.Fatal("stream started without a task ID")
		}
	}
	fixture.mu.Lock()
	calls := append([]string(nil), fixture.cleanupCalls...)
	fixture.mu.Unlock()
	if len(calls) != 1 || calls[0] != assistantDeleteAppletMethod {
		t.Fatalf("no task means no cancel: %v", calls)
	}
}

func TestAssistantStreamSendFailureCleansUp(t *testing.T) {
	fixture := newAssistantFixture(t)
	fixture.streamErr = errors.New("fixture stream refusal")
	_, _, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry())
	if err == nil {
		t.Fatal("stream transport error accepted")
	}
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
}

func TestAssistantCapabilityEvidenceAndToolState(t *testing.T) {
	if assistantFunctionToolsImplemented() {
		t.Fatal("client tool mapping must stay unimplemented until evidence exists")
	}
	model := Model{ID: "fixture-model", Capabilities: map[string]bool{"capability_code_48": true}}
	if !assistantCapableModel(model) {
		t.Fatal("capability code 48 was not recognised")
	}
	model.Capabilities = map[string]bool{}
	if assistantCapableModel(model) {
		t.Fatal("model without capability code 48 was accepted")
	}
}

func TestAssistantClientCancellationCancelsTask(t *testing.T) {
	fixture := newAssistantFixture(t)
	fixture.streamBody = assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 2, nil, assistantPart("partial", false)),
	))
	response, decode, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry())
	if err != nil {
		t.Fatal(err)
	}
	cancelled := errors.New("fixture client disconnected")
	err = decode(response.Body, func(event Event) error {
		if event.Kind == EventText {
			return cancelled
		}
		return nil
	})
	if !errors.Is(err, cancelled) {
		t.Fatalf("client cancellation was swallowed: %v", err)
	}
	_ = response.Body.Close()
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
	aux, protected, attempts := fixture.snapshot()
	if len(protected) != 1 {
		t.Fatalf("cancellation issued %d generation submits, want 1", len(protected))
	}
	if len(attempts) != 1 {
		t.Fatalf("cancellation created %d attempts, want 1", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Usage != nil {
			t.Fatalf("cancelled attempt invented usage: %+v", attempt.Usage)
		}
	}
	for _, send := range aux {
		if send.Method == assistantStreamMethod {
			return
		}
	}
	t.Fatal("stream request was never sent")
}

// An accepted CodeAssistantOffline submit may already have executed upstream, so
// even a missing task ID or an early stream error must block the outer no-content
// replay instead of submitting a second generation.
func TestAssistantAcceptedSubmitBlocksOuterReplay(t *testing.T) {
	for _, test := range []struct {
		name         string
		initBody     []byte
		streamErr    error
		expectCancel bool
	}{
		{name: "missing_task_id", initBody: []byte(`[]`)},
		{name: "stream_error", streamErr: errors.New("fixture stream refusal"), expectCancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAssistantFixture(t)
			if test.initBody != nil {
				fixture.initBody = test.initBody
			}
			fixture.streamErr = test.streamErr
			if _, _, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry()); err == nil {
				t.Fatal("failure path unexpectedly succeeded")
			}
			if !fixture.progress.Load() {
				t.Fatal("accepted submit did not block outer replay")
			}
			if test.expectCancel {
				fixture.waitCleanup(t, assistantCancelMethod)
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
		})
	}
}

var assistantUUIDPattern = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-4[0-9A-F]{3}-[89AB][0-9A-F]{3}-[0-9A-F]{12}$`)

func TestAssistantRequestIDFormat(t *testing.T) {
	for index := 0; index < 32; index++ {
		id, err := assistantRequestID()
		if err != nil {
			t.Fatal(err)
		}
		if !assistantUUIDPattern.MatchString(id) {
			t.Fatalf("request ID is not an uppercase hyphenated UUID v4: %q", id)
		}
	}
}

// Cleanup must run under the request's own lease even after the client
// disconnects, so it never re-acquires or borrows another account.
func TestAssistantCleanupKeepsOriginalLeaseAfterCancellation(t *testing.T) {
	pool, account := strictFixturePool(t)
	account.Models[0].Capabilities["capability_code_48"] = true
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild, ChannelAssistant})
	lease, _, err := pool.TryAcquireFor(context.Background(), AccountSelection{ModelID: account.Models[0].ID, Method: "generateContent", Channel: ChannelAssistant, StrictChannel: true})
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	fixture := newAssistantFixture(t)
	base, cancel := context.WithCancel(fixture.ctx)
	fixture.ctx = ContextWithAccountLease(base, lease)
	fixture.streamBody = assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 2, nil, assistantPart("partial", false)),
	))
	response, decode, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry())
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the client is gone before the stream finishes
	if err := decode(response.Body, func(Event) error { return nil }); err == nil {
		t.Fatal("canceled stream decoded as success")
	}
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
	_ = response.Body.Close()

	fixture.mu.Lock()
	cleanupLease := fixture.cleanupLease
	cleanupCtxErr := fixture.cleanupCtxErr
	fixture.mu.Unlock()
	if cleanupLease != lease {
		t.Fatal("cleanup did not keep the original account lease")
	}
	if cleanupCtxErr != nil {
		t.Fatalf("cleanup context was canceled: %v", cleanupCtxErr)
	}
}

// A failed cleanup only logs; it must not become a second generation, retry or
// touch any resource this request did not create.
func TestAssistantCleanupFailureOnlyLogs(t *testing.T) {
	fixture := newAssistantFixture(t)
	fixture.cleanupFail = true
	fixture.streamBody = assistantStreamBytes(assistantMessage(
		assistantStep("model", 1, 2, nil, assistantPart("partial", false)),
	))
	response, decode, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry())
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(response.Body, func(Event) error { return nil }); err == nil {
		t.Fatal("unfinished stream decoded as success")
	}
	_ = response.Body.Close()
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
	aux, protected, attempts := fixture.snapshot()
	if len(protected) != 1 || len(attempts) != 1 {
		t.Fatalf("cleanup failure changed generation accounting: protected=%d attempts=%d", len(protected), len(attempts))
	}
	for _, send := range aux {
		switch send.Method {
		case assistantCreateAppletMethod, assistantStreamMethod, assistantCancelMethod, assistantDeleteAppletMethod:
		default:
			t.Fatalf("cleanup touched unrelated RPC %s", send.Method)
		}
	}
}
