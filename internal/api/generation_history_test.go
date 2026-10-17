package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

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

func TestNativeSignatureUnknownOriginRejectedBeforeGeneration(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)
	body := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"real","args":{}},"thoughtSignature":"external-unknown-fixture"}]},{"role":"user","parts":[{"text":"continue"}]}]}`
	response := doJSON(t, handler, "POST", "/v1beta/models/build/gemini-4-argon:generateContent", body)
	if response.Code != 400 || service.generateCount() != 0 {
		t.Fatal(response.Code, response.Body.String())
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
