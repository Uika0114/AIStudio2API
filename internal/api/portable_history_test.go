package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func withoutHistorySignatures(c []aistudio.Content) []aistudio.Content {
	c = cloneSignatureContents(c)
	for i := range c {
		for j := range c[i].Parts {
			p := &c[i].Parts[j]
			p.ThoughtSignature = ""
			if p.FunctionCall != nil {
				p.FunctionCall.ThoughtSignature = ""
			}
		}
	}
	return c
}

func TestUnknownToolSignatureConversion(t *testing.T) {
	for _, tc := range []struct {
		name, id, base, channel, sig, part, want string
		wantErr                                  bool
	}{
		{"unknown_tool", "foreign", "model", "playground", "old-unknown", "", "", false},
		{"unknown_part", "foreign", "model", "playground", "", "old-unknown", "", false},
		{"unknown_both", "foreign", "model", "playground", "old-unknown", "old-unknown", "", false},
		{"unknown_no_id", "", "model", "playground", "old-unknown", "", "", false},
		{"unknown_build", "foreign", "model", "build", "old-unknown", "", "", false},
		{"known_preserved", "foreign", "model", "playground", "known", "", "known", false},
		{"cached_part_restore", "cached", "model", "playground", "", "old-unknown", "known", false},
		{"cached_restore", "cached", "model", "playground", "old-unknown", "", "known", false},
		{"idless_known_cross_channel", "", "model", "build", "known", "", "known", true},
		{"idless_known_cross_model", "", "other", "playground", "known", "", "known", true},
		{"known_call_cross_model", "cached", "other", "playground", "old-unknown", "", "old-unknown", true},
		{"known_call_cross_channel", "cached", "model", "build", "old-unknown", "", "old-unknown", true},
		{"strict_auto", "cached", "model", "", "known", "", "known", true},
		{"assistant_unknown", "foreign", "model", "assistant", "old-unknown", "", "old-unknown", true},
		{"conflicting", "foreign", "model", "playground", "old-unknown", "known", "old-unknown", true},
		{"legacy_unknown", "foreign", "model", "", "old-unknown", "", "old-unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newThoughtSignatureStore()
			sel := aistudio.ModelSelection{BaseModel: "model", Channel: aistudio.ChannelPlayground}
			store.Remember([]aistudio.FunctionCall{{ID: "cached", Name: "lookup", Arguments: json.RawMessage("{\"value\":1}"), ThoughtSignature: "known"}}, sel, sel.Channel, sel.BaseModel)
			c := []aistudio.Content{
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "before"}}},
				{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{Text: "assistant text"}, {FunctionCall: &aistudio.FunctionCall{ID: tc.id, Name: "lookup", Arguments: json.RawMessage("{\"value\":1}"), ThoughtSignature: tc.sig}, ThoughtSignature: tc.part}}},
				{Role: aistudio.RoleTool, Parts: []aistudio.Part{{FunctionResult: &aistudio.FunctionResult{ID: tc.id, Name: "lookup", Content: json.RawMessage("{\"result\":\"done\"}")}}}},
			}
			semantic := withoutHistorySignatures(c)
			err := store.RestoreChecked(c, aistudio.ModelSelection{BaseModel: tc.base, Channel: aistudio.Channel(tc.channel)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v expected error=%v", err, tc.wantErr)
			}
			if got := c[1].Parts[1].FunctionCall.ThoughtSignature; got != tc.want {
				t.Fatalf("signature %q != %q", got, tc.want)
			}
			if !reflect.DeepEqual(semantic, withoutHistorySignatures(c)) {
				t.Fatal("message/tool semantics changed")
			}
		})
	}
	t.Run("non_tool_unknown_remains_rejected", func(t *testing.T) {
		c := []aistudio.Content{{Parts: []aistudio.Part{{Text: "keep", ThoughtSignature: "unknown-non-tool"}}}}
		if err := newThoughtSignatureStore().RestoreChecked(c, aistudio.ModelSelection{BaseModel: "model", Channel: aistudio.ChannelBuild}); err == nil || c[0].Parts[0].ThoughtSignature != "unknown-non-tool" || c[0].Parts[0].Text != "keep" {
			t.Fatal("non-tool state silently removed")
		}
	})
}

func TestUnknownToolSignatureAcrossProtocols(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, protocol := range []string{"chat", "responses", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf(`%s/%s/%t`, channel, protocol, stream), func(t *testing.T) {
					service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(channel, true)}
					handler := newRoutingTestHandler(t, service)
					model := string(channel) + "/gemini-4-argon"
					var path, body string
					switch protocol {
					case "chat":
						path = "/v1/chat/completions"
						body = fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"before"},{"role":"assistant","content":"assistant text","tool_calls":[{"id":"call-real","type":"function","function":{"name":"real","arguments":"{\"value\":1}"},"extra_content":{"google":{"thought_signature":"old-unknown"}}}]},{"role":"tool","tool_call_id":"call-real","content":"done"}]}`, model, stream)
					case "responses":
						path = "/v1/responses"
						body = fmt.Sprintf(`{"model":%q,"stream":%t,"input":[{"role":"user","content":"before"},{"type":"reasoning","encrypted_content":"old-unknown","summary":[]},{"type":"function_call","call_id":"call-real","name":"real","arguments":"{\"value\":1}"},{"type":"function_call_output","call_id":"call-real","output":"done"}]}`, model, stream)
					case "gemini":
						action := "generateContent"
						if stream {
							action = "streamGenerateContent"
						}
						path = "/v1beta/models/" + model + ":" + action
						body = `{"contents":[{"role":"user","parts":[{"text":"before"}]},{"role":"model","parts":[{"text":"assistant text"},{"functionCall":{"id":"call-real","name":"real","args":{"value":1}},"thoughtSignature":"old-unknown"}]},{"role":"user","parts":[{"functionResponse":{"id":"call-real","name":"real","response":{"result":"done"}}}]}]}`
					}
					resp := doJSON(t, handler, "POST", path, body)
					if resp.Code != 200 || !strings.Contains(resp.Body.String(), "final answer") {
						t.Fatalf("%d %s", resp.Code, resp.Body.String())
					}
					req := service.lastGenerate(t)
					if req.Selection.Channel != channel || req.Selection.RequestedModel != model {
						t.Fatal("route changed")
					}
					var calls, results int
					var sawBefore bool
					for _, c := range req.Contents {
						for _, p := range c.Parts {
							if p.Text == "before" {
								sawBefore = true
							}
							if f := p.FunctionCall; f != nil {
								calls++
								if f.ID != "call-real" || f.Name != "real" || string(f.Arguments) != "{\"value\":1}" || f.ThoughtSignature != "" || p.ThoughtSignature != "" {
									t.Fatalf("call changed %+v %+v", f, p)
								}
							}
							if f := p.FunctionResult; f != nil {
								results++
								if f.ID != "call-real" || !strings.Contains(string(f.Content), "done") {
									t.Fatalf("result changed %+v", f)
								}
							}
						}
					}
					if calls != 1 || results != 1 || !sawBefore {
						t.Fatal("lost messages or tool pair")
					}
				})
			}
		}
	}
}

func TestPortableHistoryAnthropicBoundary(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "playground/gemini-4-argon"} {
		t.Run(model, func(t *testing.T) {
			service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(aistudio.ChannelPlayground, true)}
			handler := newRoutingTestHandler(t, service)
			body := fmt.Sprintf(`{"model":%q,"max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"fixture","signature":"old-unknown"},{"type":"tool_use","id":"call-real","name":"real","input":{"value":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-real","content":"done"}]}]}`, model)
			resp := doJSON(t, handler, "POST", "/v1/messages", body)
			if strings.Contains(model, "/") {
				if resp.Code != 400 || service.generateCount() != 0 {
					t.Fatal("routed Claude unexpectedly accepted")
				}
				return
			}
			if resp.Code != 200 {
				t.Fatal(resp.Code, resp.Body.String())
			}
			req := service.lastGenerate(t)
			found := false
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if f := p.FunctionCall; f != nil {
						found = true
						if f.ThoughtSignature != "old-unknown" || f.ID != "call-real" || f.Name != "real" {
							t.Fatal("Claude behavior changed")
						}
					}
				}
			}
			if !found {
				t.Fatal("missing Claude tool")
			}
		})
	}
}

func TestUnknownToolSignatureAfterCacheLoss(t *testing.T) {
	selection := aistudio.ModelSelection{BaseModel: "model", Channel: aistudio.ChannelPlayground}
	original := aistudio.FunctionCall{ID: "old-call", Name: "lookup", Arguments: json.RawMessage("{}"), ThoughtSignature: "old-signature"}
	for _, loss := range []string{"restart", "eviction"} {
		t.Run(loss, func(t *testing.T) {
			store := newThoughtSignatureStore()
			store.Remember([]aistudio.FunctionCall{original}, selection, selection.Channel, selection.BaseModel)
			if loss == "restart" {
				store = newThoughtSignatureStore()
			} else {
				for i := 0; i < thoughtSignatureCapacity; i++ {
					call := original
					call.ID = fmt.Sprintf(`new-%d`, i)
					call.ThoughtSignature = fmt.Sprintf(`signature-%d`, i)
					store.Remember([]aistudio.FunctionCall{call}, selection, selection.Channel, selection.BaseModel)
				}
			}
			if len(store.origins[sha256.Sum256([]byte(original.ThoughtSignature))]) != 0 {
				t.Fatal("fixture did not lose provenance")
			}
			call := original
			contents := []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{FunctionCall: &call}}}}
			if err := store.RestoreChecked(contents, selection); err != nil || call.ThoughtSignature != "" {
				t.Fatalf("portable restore after %s: %v", loss, err)
			}
			if _, exists := store.origins[sha256.Sum256([]byte(original.ThoughtSignature))]; exists {
				t.Fatal("unknown signature registered as verified")
			}
		})
	}
}

func TestPortableHistoryDoesNotMutateInput(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		t.Run(string(channel), func(t *testing.T) {
			service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(channel, true)}
			server := &server{service: service, thoughtSignatures: newThoughtSignatureStore()}
			original := []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{
				{Text: "unchanged"},
				{ThoughtSignature: "old-unknown", FunctionCall: &aistudio.FunctionCall{ID: "call-real", Name: "real", Arguments: json.RawMessage("{\"value\":1}"), ThoughtSignature: "old-unknown"}},
			}}}
			before, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			request := aistudio.GenerateRequest{Model: string(channel) + "/gemini-4-argon", Contents: original}
			for range 2 {
				events, err := server.generateWithHistory(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				for event := range events {
					if event.Err != nil {
						t.Fatal(event.Err)
					}
				}
			}
			after, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("input or shared history mutated")
			}
			received := service.lastGenerate(t)
			if received.Contents[0].Parts[1].ThoughtSignature != "" || received.Contents[0].Parts[1].FunctionCall.ThoughtSignature != "" {
				t.Fatal("unknown signature was forwarded")
			}
			if !reflect.DeepEqual(withoutHistorySignatures(original), received.Contents) {
				t.Fatal("request-copy semantics changed")
			}
			if service.generateCount() != 2 {
				t.Fatal("conversion introduced extra generation")
			}
		})
	}
}

func TestPortableHistoryResponsesBackgroundAndCompaction(t *testing.T) {
	const input = `[{"role":"user","content":"before"},{"type":"reasoning","encrypted_content":"old-unknown","summary":[]},{"type":"function_call","call_id":"call-real","name":"real","arguments":"{\"value\":1}"},{"type":"function_call_output","call_id":"call-real","output":"done"}]`
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, mode := range []string{"background", "compaction"} {
			t.Run(string(channel)+"/"+mode, func(t *testing.T) {
				service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: toolLoopEvents(channel, true)}
				model := string(channel) + "/gemini-4-argon"
				if mode == "background" {
					handler := newRoutingTestHandler(t, service)
					response := doJSON(t, handler, "POST", "/v1/responses", fmt.Sprintf(`{"model":%q,"background":true,"input":%s}`, model, input))
					if response.Code != 200 {
						t.Fatal(response.Code, response.Body.String())
					}
					var queued map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &queued); err != nil {
						t.Fatal(err)
					}
					id, _ := queued["id"].(string)
					if id == "" {
						t.Fatal("missing background response ID")
					}
					var final map[string]any
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						response = doJSON(t, handler, "GET", "/v1/responses/"+id, "")
						if err := json.Unmarshal(response.Body.Bytes(), &final); err != nil {
							t.Fatal(err)
						}
						if final["status"] == "completed" || final["status"] == "failed" {
							break
						}
						time.Sleep(time.Millisecond)
					}
					if final["status"] != "completed" {
						t.Fatalf("background result: %+v", final)
					}
				} else {
					s := &server{service: service, thoughtSignatures: newThoughtSignatureStore()}
					var items []json.RawMessage
					if err := json.Unmarshal([]byte(input), &items); err != nil {
						t.Fatal(err)
					}
					summary, _, err := s.compactionSummary(context.Background(), responsesRequest{Model: model}, items, "", "portable-compaction")
					if err != nil || summary != "final answer" {
						t.Fatalf("summary=%q err=%v", summary, err)
					}
				}
				if service.generateCount() != 1 {
					t.Fatal("history recovery replayed generation")
				}
				request := service.lastGenerate(t)
				if request.Selection.Channel != channel {
					t.Fatal("route changed")
				}
				var calls, results int
				for _, content := range request.Contents {
					for _, part := range content.Parts {
						if call := part.FunctionCall; call != nil {
							calls++
							if call.ID != "call-real" || call.Name != "real" || string(call.Arguments) != `{"value":1}` || call.ThoughtSignature != "" || part.ThoughtSignature != "" {
								t.Fatal("tool semantics or signature changed incorrectly")
							}
						}
						if result := part.FunctionResult; result != nil {
							results++
							if result.ID != "call-real" || !strings.Contains(string(result.Content), "done") {
								t.Fatal("tool result changed")
							}
						}
					}
				}
				if calls != 1 || results != 1 {
					t.Fatal("tool pair lost")
				}
			})
		}
	}
}

func TestRoutedSignatureSurvivesOriginEviction(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild, aistudio.ChannelAssistant} {
		t.Run(string(channel), func(t *testing.T) {
			store := newThoughtSignatureStore()
			selection := aistudio.ModelSelection{BaseModel: "model", Channel: channel}
			original := aistudio.FunctionCall{ID: "cached", Name: "lookup", Arguments: json.RawMessage(`{"value":1}`), ThoughtSignature: "cached-signature"}
			store.Remember([]aistudio.FunctionCall{original}, selection, selection.Channel, selection.BaseModel)
			for i := 0; i < thoughtSignatureCapacity; i++ {
				call := aistudio.FunctionCall{Name: "idless", ThoughtSignature: fmt.Sprintf("idless-%d", i)}
				store.Remember([]aistudio.FunctionCall{call}, selection, selection.Channel, selection.BaseModel)
			}
			key := routedThoughtSignatureKey(selection.BaseModel, string(selection.Channel), thoughtSignatureKey(original))
			if store.routed[key] != original.ThoughtSignature || len(store.origins[sha256.Sum256([]byte(original.ThoughtSignature))]) != 0 {
				t.Fatal("fixture must retain routed history but lose its separate origin entry")
			}
			for _, signature := range []string{"", "unknown", original.ThoughtSignature} {
				call := original
				call.ThoughtSignature = signature
				contents := []aistudio.Content{{Parts: []aistudio.Part{{FunctionCall: &call}}}}
				err := store.RestoreChecked(contents, selection)
				if channel == aistudio.ChannelAssistant {
					if err == nil {
						t.Fatal("Assistant origin checks were relaxed")
					}
				} else if err != nil || call.ThoughtSignature != original.ThoughtSignature {
					t.Fatalf("same-route cache was not restored: %v", err)
				}
			}
			// The signature can be observed on another route after eviction.
			// A known mismatch still wins over the retained cache entry.
			other := aistudio.ModelSelection{BaseModel: "other-model", Channel: aistudio.ChannelBuild}
			store.Remember([]aistudio.FunctionCall{{ThoughtSignature: original.ThoughtSignature}}, other, other.Channel, other.BaseModel)
			for _, signature := range []string{"", "unknown", original.ThoughtSignature} {
				call := original
				call.ThoughtSignature = signature
				if err := store.RestoreChecked([]aistudio.Content{{Parts: []aistudio.Part{{FunctionCall: &call}}}}, selection); err == nil {
					t.Fatal("known incompatible provenance bypassed by routed cache")
				}
			}
		})
	}
}
