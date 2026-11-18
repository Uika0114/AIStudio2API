package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestSkipValidatorToolHistoryAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, remembered := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/remembered=%t", protocol, stream, remembered), func(t *testing.T) {
					channel := aistudio.ChannelPlayground
					model := "playground/gemini-4-argon"
					service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(channel, true)}
					handler := newRoutingTestHandler(t, service)
					wantSignature := ""
					if remembered {
						service.generateEvents = toolLoopEvents(channel, false)
						first := doJSON(t, handler, "POST", "/v1/chat/completions", `{"model":"playground/gemini-4-argon","messages":[{"role":"user","content":"use real"}]}`)
						if first.Code != 200 || !strings.Contains(first.Body.String(), "fixture-signature-playground") {
							t.Fatal(first.Code, first.Body.String())
						}
						service.mu.Lock()
						service.generateEvents = toolLoopEvents(channel, true)
						service.mu.Unlock()
						wantSignature = "fixture-signature-playground"
					}
					path := "/v1/chat/completions"
					body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"use real"},{"role":"assistant","tool_calls":[{"id":"call-real","type":"function","function":{"name":"real","arguments":"{\"value\":1}"},"extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}}}]},{"role":"tool","tool_call_id":"call-real","content":"done"}]}`, model, stream)
					if protocol == "gemini" {
						action := "generateContent"
						if stream {
							action = "streamGenerateContent"
						}
						path = "/v1beta/models/" + model + ":" + action
						body = `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-real","name":"real","args":{"value":1}},"thoughtSignature":"skip_thought_signature_validator"}]},{"role":"user","parts":[{"functionResponse":{"id":"call-real","name":"real","response":{"result":"done"}}}]}]}`
					}
					response := doJSON(t, handler, "POST", path, body)
					if response.Code != 200 || !strings.Contains(response.Body.String(), "final answer") {
						t.Fatal(response.Code, response.Body.String())
					}
					request := service.lastGenerate(t)
					if request.Selection.Channel != channel || request.Selection.RequestedModel != model {
						t.Fatalf("route changed: %+v", request.Selection)
					}
					var foundCall, foundResult bool
					for _, content := range request.Contents {
						for _, part := range content.Parts {
							if call := part.FunctionCall; call != nil {
								foundCall = true
								if call.ID != "call-real" || call.Name != "real" || string(call.Arguments) != `{"value":1}` || call.ThoughtSignature != wantSignature || part.ThoughtSignature != "" {
									t.Fatalf("tool history changed: %+v %+v", call, part)
								}
							}
							if part.FunctionResult != nil {
								foundResult = true
							}
						}
					}
					if !foundCall || !foundResult {
						t.Fatal("tool-call/result history lost")
					}
				})
			}
		}
	}
}

func TestChatSharedSignatureAfterRestart(t *testing.T) {
	const carbon, argon = "gemini-4-carbon", "gemini-4-argon"
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", channel, stream), func(t *testing.T) {
				carbonModel := aistudio.Model{ID: carbon, BaseModel: carbon}
				service := &fakeService{models: []aistudio.Model{routingTestModel(), carbonModel}}
				// A fresh handler remembers only the first post-restart response.
				handler := newRoutingTestHandler(t, service)
				known := aistudio.FunctionCall{ID: "call-carbon", Name: "lookup", Arguments: json.RawMessage(`{"value":1}`), ThoughtSignature: sharedToolSignatureFixture}
				primeChatToolSignature(t, handler, service, "playground/"+carbon, aistudio.ChannelPlayground, carbon, known, stream)
				model := string(channel) + "/" + argon
				t.Run("unknown_history", func(t *testing.T) {
					old := known
					old.ID = "call-argon-old"
					setChatHistoryEvents(service, toolLoopEvents(channel, true))
					resp := doJSON(t, handler, http.MethodPost, "/v1/chat/completions", chatToolHistoryBody(model, stream, 1, old))
					if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "final answer") || (stream && !strings.Contains(resp.Body.String(), "data: [DONE]")) || service.generateCount() != 2 {
						t.Fatalf("unknown tool history did not reach generation exactly once: %d %s", resp.Code, resp.Body.String())
					}
					req := service.lastGenerate(t)
					if req.Selection.Channel != channel || req.Selection.RequestedModel != model || req.Unary == stream {
						t.Fatalf("route or mode changed: %+v, unary=%t", req.Selection, req.Unary)
					}
					part := req.Contents[1].Parts[0]
					if call := part.FunctionCall; call == nil || call.ID != old.ID || call.Name != old.Name || string(call.Arguments) != string(old.Arguments) || call.ThoughtSignature != "" || part.ThoughtSignature != "" {
						t.Fatalf("tool history changed beyond its signature: %+v / %+v", part.FunctionCall, part)
					}
					if result := req.Contents[2].Parts[0].FunctionResult; result == nil || result.ID != old.ID || !strings.Contains(string(result.Content), "done") {
						t.Fatalf("tool result linkage lost: %+v", result)
					}
					assertPortableChatToolWire(t, req, old.ID)
				})
				t.Run("known_foreign_call_even_if_digest_matches", func(t *testing.T) {
					local := known
					local.ID = "call-argon-new"
					primeChatToolSignature(t, handler, service, model, channel, argon, local, stream)
					before := service.generateCount()
					resp := doJSON(t, handler, http.MethodPost, "/v1/chat/completions", chatToolHistoryBody(model, stream, 1, known))
					if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "tool call belongs to another model/channel") || service.generateCount() != before {
						t.Fatalf("known foreign tool history was not blocked before Generate: %d %s", resp.Code, resp.Body.String())
					}
				})
			})
		}
	}
}

func TestChatRestoresSignaturesThroughSharedBoundary(t *testing.T) {
	const base = "gemini-4-carbon"
	const alias = "gemini-4-carbon-fixture-alias"
	const channel = aistudio.ChannelPlayground
	for _, stream := range []bool{false, true} {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/strict=%t", stream, strict), func(t *testing.T) {
				catalog := aistudio.Model{ID: base, BaseModel: base, CapabilityOptions: map[string][]string{"aliases": {alias}}}
				service := &fakeService{models: []aistudio.Model{catalog, routingTestModel()}}
				s := &server{service: service, thoughtSignatures: newThoughtSignatureStore()}
				handler := http.HandlerFunc(s.handleChatCompletions)
				model, followUp := base, base
				if strict {
					model, followUp = "playground/"+base, "playground/"+alias
				}
				call := aistudio.FunctionCall{ID: "call-cached", Name: "lookup", Arguments: json.RawMessage(`{"value":1}`), ThoughtSignature: "fixture-cached-signature"}
				primeChatToolSignature(t, handler, service, model, channel, base, call, stream)

				store := s.thoughtSignatures
				store.mu.Lock()
				legacySize, legacySignature := len(store.legacy), store.legacy[thoughtSignatureKey(call)]
				store.mu.Unlock()
				if strict && legacySize != 0 || !strict && legacySignature != call.ThoughtSignature {
					t.Fatalf("legacy contents do not match route policy: size=%d, signature=%q", legacySize, legacySignature)
				}

				input := call
				input.ThoughtSignature = ""
				final := toolLoopEvents(channel, true)
				for i := range final {
					final[i].ProviderModel = base
				}
				setChatHistoryEvents(service, final)
				if strict {
					for _, other := range []string{"playground/gemini-4-argon", base} {
						blocked := doJSON(t, handler, http.MethodPost, "/v1/chat/completions", chatToolHistoryBody(other, stream, 1, input))
						if blocked.Code != http.StatusBadRequest || service.generateCount() != 1 {
							t.Fatalf("omitted strict signature crossed to %s: %d %s", other, blocked.Code, blocked.Body.String())
						}
					}
				}
				resp := doJSON(t, handler, http.MethodPost, "/v1/chat/completions", chatToolHistoryBody(followUp, stream, 2, input))
				if resp.Code != http.StatusOK || strings.Count(resp.Body.String(), "final answer") != 2 || (stream && !strings.Contains(resp.Body.String(), "data: [DONE]")) {
					t.Fatalf("two-choice history replay did not finish: %d %s", resp.Code, resp.Body.String())
				}
				service.mu.Lock()
				defer service.mu.Unlock()
				if len(service.generateReqs) != 3 {
					t.Fatalf("want one initial and two follow-up requests, got %d", len(service.generateReqs))
				}
				a, b := service.generateReqs[1], service.generateReqs[2]
				left, right := a.Contents[1].Parts[0].FunctionCall, b.Contents[1].Parts[0].FunctionCall
				if left == nil || right == nil || a.ID == b.ID || left == right || len(left.Arguments) == 0 || len(right.Arguments) == 0 || &left.Arguments[0] == &right.Arguments[0] {
					t.Fatal("choice tool history must not share mutable inputs")
				}
				for _, req := range []aistudio.GenerateRequest{a, b} {
					got := req.Contents[1].Parts[0].FunctionCall
					if req.Selection.BaseModel != base || req.Selection.RequestedModel != followUp || req.Selection.Strict() != strict || got.ThoughtSignature != call.ThoughtSignature || got.ID != call.ID || got.Name != call.Name || string(got.Arguments) != string(call.Arguments) {
						t.Fatalf("choice lost canonical route or tool history: %+v / %+v", req.Selection, got)
					}
				}
			})
		}
	}
}

func chatToolHistoryBody(model string, stream bool, choices int, call aistudio.FunctionCall) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"n":%d,"messages":[{"role":"user","content":"use lookup"},{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q},"extra_content":{"google":{"thought_signature":%q}}}]},{"role":"tool","tool_call_id":%q,"content":"done"}]}`, model, stream, choices, call.ID, call.Name, string(call.Arguments), call.ThoughtSignature, call.ID)
}

func setChatHistoryEvents(service *fakeService, events []aistudio.Event) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.generateEvents = events
}

func primeChatToolSignature(t *testing.T, handler http.Handler, service *fakeService, requested string, channel aistudio.Channel, provider string, call aistudio.FunctionCall, stream bool) {
	t.Helper()
	events := toolLoopEvents(channel, false)
	events[0].ToolCall, events[0].ThoughtSignature = &call, call.ThoughtSignature
	for i := range events {
		events[i].ProviderModel = provider
	}
	setChatHistoryEvents(service, events)
	resp := doJSON(t, handler, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"use lookup"}]}`, requested, stream))
	body := resp.Body.String()
	if resp.Code != http.StatusOK || !strings.Contains(body, call.ID) || !strings.Contains(body, `"thought_signature":`+fmt.Sprintf("%q", call.ThoughtSignature)) || !strings.Contains(body, `"finish_reason":"tool_calls"`) || (stream && !strings.Contains(body, "data: [DONE]")) {
		t.Fatalf("expected projected tool event with signature: %d %s", resp.Code, body)
	}
}

func assertPortableChatToolWire(t *testing.T, request aistudio.GenerateRequest, callID string) {
	t.Helper()
	const marker = "skip_thought_signature_validator"
	defaults := aistudio.GenerationDefaults{MaxOutputTokens: 32}
	if request.Selection.Channel == aistudio.ChannelPlayground {
		body, err := aistudio.EncodeGenerateContentRequest(request, defaults, aistudio.RequestContext{})
		if err != nil {
			t.Fatal(err)
		}
		var wire []any
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		part := wire[1].([]any)[1].([]any)[0].([]any)[0].([]any)
		if len(part) <= 14 || part[14] != marker {
			t.Fatalf("wrong Playground signature: %s", body)
		}
		wantCall := fmt.Sprintf(`["lookup",[[["value",[null,1]]]],%q]`, callID)
		gotCall, err := json.Marshal(part[10])
		if err != nil || string(gotCall) != wantCall {
			t.Fatalf("wrong Playground tool call: %s", body)
		}
		return
	}
	_, body, err := aistudio.EncodeBuildGenerateRequest(request, defaults, false, request.Unary)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Contents []struct{ Parts []map[string]json.RawMessage }
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	part := wire.Contents[1].Parts[0]
	wantCall := fmt.Sprintf(`{"args":{"value":1},"id":%q,"name":"lookup"}`, callID)
	if string(part["thoughtSignature"]) != `"`+marker+`"` || string(part["functionCall"]) != wantCall {
		t.Fatalf("wrong Build tool call/signature: %s", body)
	}
}

func protocolObjects(t *testing.T, body string, stream bool) []map[string]any {
	t.Helper()
	if !stream {
		var value map[string]any
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err, body)
		}
		return []map[string]any{value}
	}
	var values []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			t.Fatal(err, data)
		}
		values = append(values, value)
	}
	return values
}

func toolLoopEvents(channel aistudio.Channel, final bool) []aistudio.Event {
	var event aistudio.Event
	if final {
		event = aistudio.Event{Kind: aistudio.EventText, Text: "final answer"}
	} else {
		event = aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "call-real", Name: "real", Arguments: json.RawMessage(`{"value":1}`), ThoughtSignature: "fixture-signature-" + string(channel)}}
	}
	event.Channel, event.ProviderModel = channel, "gemini-4-argon"
	return []aistudio.Event{event, {Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12, Source: "upstream"}, Channel: channel, ProviderModel: "gemini-4-argon"}, {Kind: aistudio.EventFinish, FinishReason: "stop", Channel: channel, ProviderModel: "gemini-4-argon"}}
}

func TestThreeProtocolRealToolLoopAndHistoryIsolation(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, protocol := range []string{"chat", "responses", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", channel, protocol, stream), func(t *testing.T) {
					service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(channel, false)}
					handler := newRoutingTestHandler(t, service)
					model := string(channel) + "/anti-truncation/gemini-4-argon"
					var path, first string
					switch protocol {
					case "chat":
						path = "/v1/chat/completions"
						first = fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"use real"}],"tools":[{"type":"function","function":{"name":"real","parameters":{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}}}]}`, model, stream)
					case "responses":
						path = "/v1/responses"
						first = fmt.Sprintf(`{"model":%q,"stream":%t,"input":"use real","tools":[{"type":"function","name":"real","parameters":{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}}]}`, model, stream)
					case "gemini":
						action := "generateContent"
						if stream {
							action = "streamGenerateContent"
						}
						path = "/v1beta/models/" + model + ":" + action
						first = `{"contents":[{"role":"user","parts":[{"text":"use real"}]}],"tools":[{"functionDeclarations":[{"name":"real","parameters":{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}}]}]}`
					}
					response := doJSON(t, handler, "POST", path, first)
					if response.Code != 200 {
						t.Fatal(response.Code, response.Body.String())
					}
					objects := protocolObjects(t, response.Body.String(), stream)
					if !strings.Contains(response.Body.String(), "call-real") || !strings.Contains(response.Body.String(), "real") {
						t.Fatal("real tool was lost", response.Body.String())
					}
					if strings.Contains(response.Body.String(), "aistudio2api_emit_v1") {
						t.Fatal("internal transport leaked")
					}
					responseID := ""
					if protocol == "responses" {
						for _, object := range objects {
							if nested, ok := object["response"].(map[string]any); ok {
								object = nested
							}
							if id, ok := object["id"].(string); ok && strings.HasPrefix(id, "resp_") {
								responseID = id
							}
						}
						if responseID == "" {
							t.Fatal("missing Responses ID", response.Body.String())
						}
					}
					if protocol == "gemini" {
						for _, object := range objects {
							if got, ok := object["modelVersion"].(string); ok && got != model {
								t.Fatalf("public model lost prefix: %q", got)
							}
						}
					}
					var second string
					switch protocol {
					case "chat":
						second = fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"use real"},{"role":"assistant","tool_calls":[{"id":"call-real","type":"function","function":{"name":"real","arguments":"{\"value\":1}"}}]},{"role":"tool","tool_call_id":"call-real","content":"done"}]}`, model, stream)
					case "responses":
						second = fmt.Sprintf(`{"previous_response_id":%q,"stream":%t,"input":[{"type":"function_call_output","call_id":"call-real","output":"done"}]}`, responseID, stream)
					case "gemini":
						second = fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"use real"}]},{"role":"model","parts":[{"functionCall":{"id":"call-real","name":"real","args":{"value":1}},"thoughtSignature":%q}]},{"role":"user","parts":[{"functionResponse":{"id":"call-real","name":"real","response":{"result":"done"}}}]}]}`, "fixture-signature-"+string(channel))
					}
					service.mu.Lock()
					service.generateEvents = toolLoopEvents(channel, true)
					service.mu.Unlock()
					response = doJSON(t, handler, "POST", path, second)
					if response.Code != 200 || !strings.Contains(response.Body.String(), "final answer") {
						t.Fatal(response.Code, response.Body.String())
					}
					request := service.lastGenerate(t)
					if request.Selection.Channel != channel || !request.Selection.AntiTruncation || request.Selection.RequestedModel != model {
						t.Fatalf("lost inherited policy: %+v", request.Selection)
					}
					var foundCall, foundResult bool
					for _, content := range request.Contents {
						for _, part := range content.Parts {
							if call := part.FunctionCall; call != nil && call.Name == "real" {
								foundCall = true
								if call.ID != "call-real" || string(call.Arguments) != `{"value":1}` || (call.ThoughtSignature != "fixture-signature-"+string(channel) && part.ThoughtSignature != "fixture-signature-"+string(channel)) {
									t.Fatalf("tool identity/args/signature changed: %+v %+v", call, part)
								}
							}
							if result := part.FunctionResult; result != nil {
								foundResult = true
							}
						}
					}
					if !foundCall || !foundResult {
						t.Fatal("tool loop history lost")
					}
					// The same opaque history must not be silently reused on another channel.
					other := aistudio.ChannelBuild
					if channel == other {
						other = aistudio.ChannelPlayground
					}
					otherModel := string(other) + "/anti-truncation/gemini-4-argon"
					crossPath, crossBody := path, second
					if protocol == "gemini" {
						crossPath = strings.Replace(path, model, otherModel, 1)
					} else if protocol == "responses" {
						crossBody = fmt.Sprintf(`{"model":%q,"previous_response_id":%q,"input":"continue"}`, otherModel, responseID)
					} else {
						crossBody = strings.Replace(second, model, otherModel, 1)
					}
					before := service.generateCount()
					blocked := doJSON(t, handler, "POST", crossPath, crossBody)
					if blocked.Code != 400 || service.generateCount() != before {
						t.Fatal("cross-channel history sent", blocked.Code, blocked.Body.String())
					}
				})
			}
		}
	}
}

func TestNativeSignatureUnknownOriginToolBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, part          string
		wantCode, wantCalls int
	}{
		{"tool", `{"functionCall":{"name":"real","args":{}},"thoughtSignature":"external-unknown-fixture"}`, 200, 1},
		{"text", `{"text":"keep","thoughtSignature":"external-unknown-fixture"}`, 400, 0},
		{"signature_only", `{"thoughtSignature":"external-unknown-fixture"}`, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(aistudio.ChannelBuild, true)}
			handler := newRoutingTestHandler(t, service)
			body := fmt.Sprintf(`{"contents":[{"role":"model","parts":[%s]},{"role":"user","parts":[{"text":"continue"}]}]}`, tc.part)
			response := doJSON(t, handler, "POST", "/v1beta/models/build/gemini-4-argon:generateContent", body)
			if response.Code != tc.wantCode || service.generateCount() != tc.wantCalls {
				t.Fatal(response.Code, response.Body.String())
			}
			if tc.wantCalls != 0 {
				part := service.lastGenerate(t).Contents[0].Parts[0]
				if part.ThoughtSignature != "" || part.FunctionCall == nil || part.FunctionCall.Name != "real" || part.FunctionCall.ThoughtSignature != "" {
					t.Fatal("native unknown tool was not preserved as portable history")
				}
			}
		})
	}
}

func TestSignatureOriginsWithoutCallIDAndLegacyCompatibility(t *testing.T) {
	store := newThoughtSignatureStore()
	selection, _ := aistudio.ParseModelSelection("build/gemini-4-argon")
	call := aistudio.FunctionCall{Name: "real", Arguments: json.RawMessage(`{}`), ThoughtSignature: "native-no-id"}
	store.Remember([]aistudio.FunctionCall{call}, selection, aistudio.ChannelBuild, "gemini-4-argon")
	contents := []aistudio.Content{{Parts: []aistudio.Part{{FunctionCall: &call}}}}
	if err := store.RestoreChecked(contents, selection); err != nil {
		t.Fatal(err)
	}
	other, _ := aistudio.ParseModelSelection("playground/gemini-4-argon")
	if err := store.RestoreChecked(contents, other); err == nil {
		t.Fatal("id-less signature crossed channel")
	}
	auto, _ := aistudio.ParseModelSelection("gemini-4-argon")
	if err := store.RestoreChecked(contents, auto); err == nil {
		t.Fatal("strict history escaped into auto")
	}
	call.ThoughtSignature = "old-unknown-signature"
	if err := store.RestoreChecked(contents, auto); err != nil {
		t.Fatal("legacy auto behavior changed", err)
	}
}

func TestUsageAggregationPreservesProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parts  []generationResult
		source string
	}{
		{"upstream", []generationResult{{usage: &aistudio.Usage{TotalTokens: 3, Source: "upstream"}}, {usage: &aistudio.Usage{TotalTokens: 5, Source: "upstream"}}}, "upstream"},
		{"estimated", []generationResult{{usage: &aistudio.Usage{Source: "estimated"}}}, "estimated"},
		{"mixed", []generationResult{{usage: &aistudio.Usage{Source: "upstream"}}, {usage: &aistudio.Usage{Source: "estimated"}}}, "mixed"},
		{"partly_unknown", []generationResult{{usage: &aistudio.Usage{Source: "upstream"}}, {}}, "mixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sumUsage(tc.parts)
			if got == nil || got.Source != tc.source {
				t.Fatalf("%+v", got)
			}
		})
	}
}
