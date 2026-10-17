package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// All fixtures stop below the protected transport. No WAA, credentials,
// browsers, listener or external HTTP client is created by these tests.
type fixtureAuxiliaryRPC struct{}

func (fixtureAuxiliaryRPC) Do(context.Context, RPCRequest) (*RPCResponse, error) {
	return nil, errors.New("fixture: auxiliary network disabled")
}

type pipelineFixture struct {
	client   *Client
	ctx      context.Context
	account  *Account
	mu       sync.Mutex
	sends    []RPCRequest
	attempts map[string]GenerationAttempt
	reply    func(GenerateRequest, RPCRequest, int) ([]byte, error)
}

func newPipelineFixture(t *testing.T, channel Channel) *pipelineFixture {
	t.Helper()
	pool, account := strictFixturePool(t)
	model := account.Models[0]
	model.InputTokenLimit = 1048576
	model.OutputTokenLimit = 65536
	model.Capabilities["output_continuation"] = true
	account.Models = []Model{model}
	if err := pool.SetBuildCatalog(account.ID, []Model{model}); err != nil {
		t.Fatal(err)
	}
	lease, _, err := pool.TryAcquireFor(context.Background(), strictSelection(channel))
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	})
	fixture := &pipelineFixture{account: account, attempts: map[string]GenerationAttempt{}}
	fixture.ctx = ContextWithGenerationAttemptObserver(ContextWithAccountLease(context.Background(), lease), func(snapshot GenerationAttempt) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if previous, ok := fixture.attempts[snapshot.ID]; !ok || snapshot.Revision >= previous.Revision {
			fixture.attempts[snapshot.ID] = snapshot
		}
	})
	transport := ProtectedTransportFunc(func(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
		if err := request.Selection.ValidateChannel(generationRPCChannel(rpc.Method)); err != nil {
			return nil, err
		}
		ctx, attempt := beginPhysicalAttempt(ctx, request, rpc)
		markPhysicalSend(ctx)
		fixture.mu.Lock()
		fixture.sends = append(fixture.sends, rpc)
		index := len(fixture.sends)
		fixture.mu.Unlock()
		if request.Model != "fixture-model" || bytes.Contains(rpc.Body, []byte("playground/")) || bytes.Contains(rpc.Body, []byte("anti-truncation/")) || bytes.Contains(rpc.Body, []byte("build/fixture")) {
			t.Errorf("public selector leaked to wire: %s", rpc.Body)
		}
		body, err := fixture.reply(request, rpc, index)
		if err != nil {
			attempt.fail(err)
			return nil, err
		}
		return attempt.attach(&RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(body))}), nil
	})
	fixture.client, err = NewClient(ClientOptions{Transport: fixtureAuxiliaryRPC{}, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	entry := modelEntry{model: model, defaults: GenerationDefaults{MaxOutputTokens: 32768, ContinuedOutputTokens: 65536}}
	fixture.client.catalogs[account.ID] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{model.ID: entry}}
	return fixture
}

func fixtureResponse(t *testing.T, rpc RPCRequest, parts []Part, finish string, usage *Usage, continuation string) []byte {
	t.Helper()
	var root any
	if rpc.Method == "GenerateContent" {
		content, err := encodeContent(Content{Role: RoleAssistant, Parts: parts})
		if err != nil {
			t.Fatal(err)
		}
		candidate := make([]any, 9)
		candidate[0] = content
		switch finish {
		case "stop":
			candidate[1] = 1
		case "max_tokens":
			candidate[1] = 2
		case "safety":
			candidate[1] = 3
		case "continue":
			candidate[1] = 22
		}
		if continuation != "" {
			candidate[8] = continuation
		}
		var counts any
		if usage != nil {
			counts = []any{usage.InputTokens, usage.OutputTokens, usage.TotalTokens, usage.CachedTokens}
		}
		root = []any{[]any{[]any{[]any{candidate}, nil, counts}}}
	} else {
		wireParts := make([]any, 0, len(parts))
		for _, part := range parts {
			if part.FunctionCall != nil {
				wireParts = append(wireParts, map[string]any{"functionCall": map[string]any{"id": part.FunctionCall.ID, "name": part.FunctionCall.Name, "args": part.FunctionCall.Arguments}, "thoughtSignature": part.ThoughtSignature})
			} else {
				wireParts = append(wireParts, map[string]any{"text": part.Text, "thought": part.Thought, "thoughtSignature": part.ThoughtSignature})
			}
		}
		candidate := map[string]any{"content": map[string]any{"role": "model", "parts": wireParts}}
		switch finish {
		case "stop":
			candidate["finishReason"] = "STOP"
		case "max_tokens":
			candidate["finishReason"] = "MAX_TOKENS"
		case "safety":
			candidate["finishReason"] = "SAFETY"
		}
		response := map[string]any{"candidates": []any{candidate}}
		if usage != nil {
			response["usageMetadata"] = map[string]any{"promptTokenCount": usage.InputTokens, "candidatesTokenCount": usage.OutputTokens, "totalTokenCount": usage.TotalTokens}
		}
		body, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if rpc.Method == buildProxyUnaryMethod {
			root = []any{string(body)}
		} else {
			root = []any{[]any{[]any{string(body)}}}
		}
	}
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func fixtureTransportPart(t *testing.T, request GenerateRequest, text string) Part {
	t.Helper()
	if !request.Selection.AntiTruncation || len(request.Tools.Functions) == 0 {
		t.Fatal("anti policy/transport missing")
	}
	name := request.Tools.Functions[len(request.Tools.Functions)-1].Name
	arguments, _ := json.Marshal(map[string]string{"content": text})
	return Part{FunctionCall: &FunctionCall{ID: "internal-only-id", Name: name, Arguments: arguments, ThoughtSignature: "internal-only-signature"}, ThoughtSignature: "internal-only-signature"}
}

func (fixture *pipelineFixture) generate(t *testing.T, request GenerateRequest) []Event {
	t.Helper()
	request.ID = t.Name()
	request.AccountID = fixture.account.ID
	if len(request.Contents) == 0 {
		request.Contents = []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}}}
	}
	source, err := fixture.client.Generate(fixture.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for event := range source {
		events = append(events, event)
	}
	return events
}

func pipelineText(events []Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Kind == EventText {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}
func pipelineError(events []Event) error {
	for _, event := range events {
		if event.Kind == EventError {
			return event.Err
		}
	}
	return nil
}
func pipelineFinish(events []Event) *Event {
	for i := range events {
		if events[i].Kind == EventFinish {
			return &events[i]
		}
	}
	return nil
}
func pipelineUsage(events []Event) *Usage {
	for _, event := range events {
		if event.Kind == EventUsage {
			return event.Usage
		}
	}
	return nil
}

func TestGenerationPipelineChannelTransportMatrix(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, unary := range []bool{false, true} {
			for _, anti := range []bool{false, true} {
				name := string(channel)
				if unary {
					name += "/unary"
				} else {
					name += "/stream"
				}
				if anti {
					name += "/anti"
				} else {
					name += "/plain"
				}
				t.Run(name, func(t *testing.T) {
					fixture := newPipelineFixture(t, channel)
					text := "你好\n\"quoted\" 😀"
					fixture.reply = func(request GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
						if request.Selection.AntiTruncation != anti {
							t.Fatal("policy lost")
						}
						parts := []Part{{Text: text}}
						if anti {
							parts = []Part{fixtureTransportPart(t, request, text)}
						} else if len(request.Tools.Functions) != 0 {
							t.Fatal("ordinary mode injected a transport")
						}
						return fixtureResponse(t, rpc, parts, "stop", &Usage{InputTokens: 50, OutputTokens: 42, TotalTokens: 92}, ""), nil
					}
					model := string(channel) + "/"
					if anti {
						model += "anti-truncation/"
					}
					model += "fixture-model"
					events := fixture.generate(t, GenerateRequest{Model: model, Unary: unary})
					if err := pipelineError(events); err != nil {
						t.Fatal(err)
					}
					if pipelineText(events) != text || pipelineFinish(events) == nil {
						t.Fatalf("bad projection: %+v", events)
					}
					usage := pipelineUsage(events)
					if usage == nil || usage.TotalTokens != 92 || usage.Source != "upstream" {
						t.Fatalf("usage changed: %+v", usage)
					}
					for _, event := range events {
						if event.Channel != channel || event.ProviderModel != "fixture-model" {
							t.Fatalf("wrong physical identity: %+v", event)
						}
						if event.ToolCall != nil || event.ThoughtSignature != "" {
							t.Fatal("hidden call/signature leaked")
						}
					}
					if len(fixture.sends) != 1 || len(fixture.attempts) != 1 {
						t.Fatalf("unexpected sends/attempts: %d/%d", len(fixture.sends), len(fixture.attempts))
					}
					for _, attempt := range fixture.attempts {
						if !attempt.Sent || !attempt.ResponseReceived || attempt.Channel != channel || attempt.Usage == nil || attempt.Usage.TotalTokens != 92 || attempt.UsageSource != "upstream" || attempt.RequestedModel != model {
							t.Fatalf("audit wrong: %+v", attempt)
						}
					}
				})
			}
		}
	}
}

func TestGenerationPipelineTerminalAndUsageFailures(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, kind := range []string{"missing_finish", "invalid_content", "zero_usage", "unknown_usage", "safety", "length", "truncated_envelope"} {
			t.Run(string(channel)+"/"+kind, func(t *testing.T) {
				fixture := newPipelineFixture(t, channel)
				fixture.reply = func(request GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
					part := fixtureTransportPart(t, request, "visible")
					finish := "stop"
					usage := &Usage{InputTokens: 10, OutputTokens: 9, TotalTokens: 19}
					switch kind {
					case "missing_finish":
						finish = ""
					case "invalid_content":
						part.FunctionCall.Arguments = json.RawMessage(`{"content":42}`)
					case "zero_usage":
						usage = &Usage{}
					case "unknown_usage":
						usage = nil
					case "safety":
						finish = "safety"
					case "length":
						finish = "max_tokens"
					}
					body := fixtureResponse(t, rpc, []Part{part}, finish, usage, "")
					if kind == "truncated_envelope" {
						body = body[:len(body)-1]
					}
					return body, nil
				}
				events := fixture.generate(t, GenerateRequest{Model: string(channel) + "/anti-truncation/fixture-model"})
				failed := kind == "missing_finish" || kind == "invalid_content" || kind == "truncated_envelope"
				if failed {
					if pipelineError(events) == nil || pipelineFinish(events) != nil {
						t.Fatalf("failure became success: %+v", events)
					}
				} else if pipelineError(events) != nil || pipelineFinish(events) == nil {
					t.Fatalf("unexpected failure: %+v", events)
				}
				usage := pipelineUsage(events)
				if kind == "unknown_usage" {
					if usage == nil || usage.Source != "estimated" {
						t.Fatalf("estimate unlabelled: %+v", usage)
					}
				} else if usage == nil || usage.Source != "upstream" {
					t.Fatalf("observed usage hidden: %+v", usage)
				}
				if kind == "zero_usage" && usage.TotalTokens != 0 {
					t.Fatal("known zero became an estimate")
				}
				if kind == "safety" && pipelineFinish(events).FinishReason != "safety" {
					t.Fatal("safety laundered")
				}
				if kind == "length" && pipelineFinish(events).FinishReason != "max_tokens" {
					t.Fatal("length laundered")
				}
				if len(fixture.sends) != 1 {
					t.Fatal("projection recovery generated again")
				}
			})
		}
	}
}

func TestGenerationPipelineStopPreservesUsageAndRequiredReal(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, required := range []bool{false, true} {
			name := string(channel)
			if required {
				name += "/required"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newPipelineFixture(t, channel)
				fixture.reply = func(request GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
					return fixtureResponse(t, rpc, []Part{fixtureTransportPart(t, request, "before STOP after")}, "max_tokens", &Usage{InputTokens: 20, OutputTokens: 300, TotalTokens: 320}, ""), nil
				}
				request := GenerateRequest{Model: string(channel) + "/anti-truncation/fixture-model", Config: GenerationConfig{StopSequences: []string{" STOP"}}}
				if required {
					request.Tools = Tools{Functions: []FunctionDeclaration{{Name: "real", Parameters: json.RawMessage(`{"type":"object"}`)}}, ToolConfig: ToolConfig{Mode: "required"}}
				}
				events := fixture.generate(t, request)
				usage := pipelineUsage(events)
				if usage == nil || usage.OutputTokens != 300 || usage.Source != "upstream" {
					t.Fatalf("display text overwrote raw usage: %+v", usage)
				}
				if required {
					if pipelineError(events) == nil || pipelineFinish(events) != nil {
						t.Fatal("local stop satisfied required-real")
					}
				} else {
					if pipelineError(events) != nil || pipelineText(events) != "before" || pipelineFinish(events) == nil || pipelineFinish(events).FinishReason != "max_tokens" {
						t.Fatalf("stop/length wrong: %+v", events)
					}
				}
			})
		}
	}
}

func TestGenerationPipelineRealToolRoundTrip(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		t.Run(string(channel), func(t *testing.T) {
			fixture := newPipelineFixture(t, channel)
			fixture.reply = func(request GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
				if index == 1 {
					return fixtureResponse(t, rpc, []Part{{FunctionCall: &FunctionCall{ID: "real-call-id", Name: "real", Arguments: json.RawMessage(`{"value":1}`), ThoughtSignature: "real-signature"}, ThoughtSignature: "real-signature"}}, "stop", &Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}, ""), nil
				}
				call := request.Contents[1].Parts[0].FunctionCall
				result := request.Contents[2].Parts[0].FunctionResult
				if call == nil || call.ID != "real-call-id" || call.ThoughtSignature != "real-signature" || result == nil || result.ID != "real-call-id" {
					t.Fatal("real history changed")
				}
				return fixtureResponse(t, rpc, []Part{fixtureTransportPart(t, request, "final answer")}, "stop", &Usage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}, ""), nil
			}
			tools := Tools{Functions: []FunctionDeclaration{{Name: "real", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`)}}, ToolConfig: ToolConfig{Mode: "required"}}
			request := GenerateRequest{Model: string(channel) + "/anti-truncation/fixture-model", Tools: tools, Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}}}}
			first := fixture.generate(t, request)
			if pipelineError(first) != nil {
				t.Fatalf("first turn: %+v", first)
			}
			var call *FunctionCall
			for _, event := range first {
				if event.ToolCall != nil {
					call = event.ToolCall
				}
			}
			if call == nil || call.ID != "real-call-id" || call.ThoughtSignature != "real-signature" {
				t.Fatalf("real call/signature missing: %+v", first)
			}
			request.Tools.ToolConfig.Mode = "auto"
			request.Contents = append(request.Contents, Content{Role: RoleAssistant, Parts: []Part{{FunctionCall: call}}}, Content{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: call.ID, Name: call.Name, Content: json.RawMessage(`{"ok":true}`)}}}})
			second := fixture.generate(t, request)
			if pipelineError(second) != nil || pipelineText(second) != "final answer" || pipelineFinish(second) == nil {
				t.Fatalf("second turn: %+v", second)
			}
			if len(fixture.sends) != 2 {
				t.Fatal("unexpected logical replay")
			}
		})
	}
}
