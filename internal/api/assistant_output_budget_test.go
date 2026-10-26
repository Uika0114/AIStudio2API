package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assistantBudgetPayload(t *testing.T, model, protocol string, stream bool, budget any) (string, map[string]any) {
	t.Helper()
	request := map[string]any{"model": "assistant/" + model, "stream": stream}
	switch protocol {
	case "chat":
		request["messages"] = []any{map[string]any{"role": "user", "content": "fixture budget request"}}
		if budget != nil {
			request["max_tokens"] = budget
		}
		return "/v1/chat/completions", request
	case "responses":
		request["input"] = "fixture budget request"
		if budget != nil {
			request["max_output_tokens"] = budget
		}
		return "/v1/responses", request
	case "gemini":
		request = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "fixture budget request"}}}}}
		if budget != nil {
			request["generationConfig"] = map[string]any{"maxOutputTokens": budget}
		}
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/assistant/" + model + ":" + action, request
	}
	t.Fatal("unknown protocol")
	return "", nil
}

func budgetRequestJSON(t *testing.T, object map[string]any) string {
	t.Helper()
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func budgetResponseObjects(t *testing.T, body string, stream bool) []map[string]any {
	t.Helper()
	var result []map[string]any
	decode := func(raw string) {
		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		if object["object"] == "response" {
			result = append(result, object)
		} else if response, ok := object["response"].(map[string]any); ok {
			result = append(result, response)
		}
	}
	if stream {
		for line := range strings.SplitSeq(body, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
				decode(data)
			}
		}
	} else {
		decode(body)
	}
	if len(result) == 0 {
		t.Fatalf("no Responses objects: %s", body)
	}
	return result
}

func TestAssistantOutputBudgetResponseProjection(t *testing.T) {
	positive, zero, negative := int64(1), int64(0), int64(-1)
	for _, model := range []string{"assistant/gemini-4-argon", "models/assistant/gemini-3.8-flash", "playground/gemini-4-argon", "build/gemini-4-argon", "gemini-4-argon", "assistant/"} {
		for _, budget := range []*int64{nil, &positive, &zero, &negative} {
			t.Run(fmt.Sprintf("%s/%v", model, budget), func(t *testing.T) {
				request := responsesRequest{Model: model, MaxOutputTokens: budget, Metadata: map[string]string{"customer": "untouched"}}
				original := request.MaxOutputTokens
				for _, status := range []string{"queued", "in_progress", "completed", "failed", "cancelled"} {
					raw, err := json.Marshal(responseShell("fixture-response", 1, status, request))
					if err != nil {
						t.Fatal(err)
					}
					var shell map[string]any
					_ = json.Unmarshal(raw, &shell)
					ignored := budget != nil && *budget > 0 && model != "assistant/" && strings.Contains(model, "assistant/")
					if ignored || budget == nil {
						if shell["max_output_tokens"] != nil {
							t.Fatalf("ignored budget advertised as effective: %s", raw)
						}
					} else if shell["max_output_tokens"] != float64(*budget) {
						t.Fatalf("unrelated budget echo changed: %s", raw)
					}
					if request.MaxOutputTokens != original || !strings.Contains(string(raw), `"customer":"untouched"`) {
						t.Fatal("projection mutated input or customer metadata")
					}
				}
			})
		}
	}
}

func TestAssistantOutputBudgetThreeProtocolsRealClient(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash"} {
		for _, protocol := range []string{"chat", "responses", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", model, protocol, stream), func(t *testing.T) {
					service := newAssistantTextIntegrationForModel(t, model)
					handler := newRoutingTestHandler(t, service)
					for _, budget := range []any{nil, 1} {
						path, request := assistantBudgetPayload(t, model, protocol, stream, budget)
						response := doJSON(t, handler, "POST", path, budgetRequestJSON(t, request))
						if response.Code != 200 || !strings.Contains(response.Body.String(), "fixture text response") {
							t.Fatalf("%d: %s", response.Code, response.Body.String())
						}
						if protocol == "responses" {
							for _, object := range budgetResponseObjects(t, response.Body.String(), stream) {
								if object["max_output_tokens"] != nil {
									t.Fatal("Responses projected an ignored effective budget")
								}
							}
						}
					}
					service.mu.Lock()
					defer service.mu.Unlock()
					if len(service.bodies) != 2 || len(service.requests) != 2 {
						t.Fatalf("submit count=%d", len(service.bodies))
					}
					var normalized [2][]any
					for index, body := range service.bodies {
						if service.requests[index].Config.MaxOutputTokens != nil || service.requests[index].Model != model {
							t.Fatal("budget or wrong model reached protected submit")
						}
						if err := json.Unmarshal(body, &normalized[index]); err != nil {
							t.Fatal(err)
						}
						if len(normalized[index]) != 29 {
							t.Fatal("unexpected wire structure")
						}
						normalized[index][11], normalized[index][28] = nil, nil // Applet and request UUID only.
					}
					if !reflect.DeepEqual(normalized[0], normalized[1]) {
						t.Fatal("budget changed actual Assistant wire")
					}
				})
			}
		}
	}
}

func TestAssistantOutputBudgetAliasesAndParsing(t *testing.T) {
	cases := []struct {
		name, protocol string
		extra          map[string]any
		accepted       bool
	}{
		{"chat-positive-winner", "chat", map[string]any{"max_tokens": -1, "max_completion_tokens": 1}, true},
		{"chat-negative-winner", "chat", map[string]any{"max_tokens": 1, "max_completion_tokens": -1}, false},
		{"chat-loser-type-error", "chat", map[string]any{"max_tokens": "bad", "max_completion_tokens": 1}, false},
		{"chat-completion-only", "chat", map[string]any{"max_completion_tokens": 1}, true},
		{"chat-float", "chat", map[string]any{"max_tokens": json.RawMessage(`1.0`)}, false},
		{"chat-overflow", "chat", map[string]any{"max_tokens": json.RawMessage(`9223372036854775808`)}, false},
		{"gemini-positive-camel-winner", "gemini", map[string]any{"generationConfig": map[string]any{"max_output_tokens": "discarded", "maxOutputTokens": 1}}, true},
		{"gemini-negative-camel-winner", "gemini", map[string]any{"generationConfig": map[string]any{"max_output_tokens": 1, "maxOutputTokens": -1}}, false},
		{"gemini-snake-only", "gemini", map[string]any{"generation_config": map[string]any{"max_output_tokens": 1}}, true},
		{"responses-zero", "responses", map[string]any{"max_output_tokens": 0}, false},
		{"responses-float", "responses", map[string]any{"max_output_tokens": json.RawMessage(`1.0`)}, false},
		{"responses-null", "responses", map[string]any{"max_output_tokens": nil}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service := newAssistantTextIntegration(t)
			handler := newRoutingTestHandler(t, service)
			path, request := assistantBudgetPayload(t, "gemini-4-argon", test.protocol, false, nil)
			for key, value := range test.extra {
				request[key] = value
			}
			response := doJSON(t, handler, "POST", path, budgetRequestJSON(t, request))
			if (response.Code == 200) != test.accepted {
				t.Fatalf("%d: %s", response.Code, response.Body.String())
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if !test.accepted {
				if len(service.requests) != 0 {
					t.Fatal("bad budget generated")
				}
				for _, method := range service.auxMethods {
					if method != "ListModels" {
						t.Fatalf("bad budget created resources: %s", method)
					}
				}
			} else if len(service.requests) != 1 {
				t.Fatal("accepted request failed to generate exactly once")
			}
		})
	}
}

func TestAssistantOutputBudgetResponsesStoredAndBackground(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, budget := range []int{1, 0, -1} {
			t.Run(fmt.Sprintf("stream=%t/budget=%d", stream, budget), func(t *testing.T) {
				service := newAssistantTextIntegration(t)
				handler := newRoutingTestHandler(t, service)
				_, request := assistantBudgetPayload(t, "gemini-4-argon", "responses", stream, budget)
				request["background"] = true
				result := doJSON(t, handler, "POST", "/v1/responses", budgetRequestJSON(t, request))
				if result.Code != 200 {
					t.Fatalf("queued response was retroactively rejected: %d %s", result.Code, result.Body.String())
				}
				objects := budgetResponseObjects(t, result.Body.String(), stream)
				id, _ := objects[0]["id"].(string)
				if id == "" {
					t.Fatal("queued response has no id")
				}
				var final map[string]any
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					get := doJSON(t, handler, "GET", "/v1/responses/"+id, "")
					if get.Code == 200 {
						_ = json.Unmarshal(get.Body.Bytes(), &final)
						if final["status"] == "completed" || final["status"] == "failed" {
							break
						}
					}
					time.Sleep(time.Millisecond)
				}
				wantStatus := "failed"
				if budget > 0 {
					wantStatus = "completed"
				}
				if final["status"] != wantStatus {
					t.Fatalf("final status: %+v", final)
				}
				if budget > 0 {
					for _, object := range append(objects, final) {
						if object["max_output_tokens"] != nil {
							t.Fatal("queued/stream/GET advertised ignored budget")
						}
					}
				}
				service.mu.Lock()
				defer service.mu.Unlock()
				if budget <= 0 && len(service.requests) != 0 {
					t.Fatal("invalid background budget generated")
				}
				if budget > 0 && len(service.requests) != 1 {
					t.Fatal("GET generated or background duplicated")
				}
			})
		}
	}
}
