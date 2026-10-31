package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assistantSamplingPayload(t *testing.T, model, protocol string, stream bool) (string, map[string]any) {
	path, request := assistantBudgetPayload(t, model, protocol, stream, 1)
	if protocol == "gemini" {
		config := request["generationConfig"].(map[string]any)
		config["temperature"], config["topP"], config["topK"], config["seed"] = 0, 1, 0, json.RawMessage(`-9223372036854775808`)
	} else {
		request["temperature"], request["top_p"] = 0, 1
		if protocol == "chat" {
			request["seed"] = json.RawMessage(`9223372036854775807`)
		}
	}
	return path, request
}

func assertSamplingNull(t *testing.T, object map[string]any) {
	t.Helper()
	if object["temperature"] != nil || object["top_p"] != nil || object["max_output_tokens"] != nil {
		t.Fatalf("ignored controls advertised as effective: %+v", object)
	}
}

func TestAssistantSamplingThreeProtocolsRealWire(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash"} {
		for _, protocol := range []string{"chat", "responses", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", model, protocol, stream), func(t *testing.T) {
					service := newAssistantTextIntegrationForModel(t, model)
					handler := newRoutingTestHandler(t, service)
					_, plain := assistantBudgetPayload(t, model, protocol, stream, nil)
					path, sampled := assistantSamplingPayload(t, model, protocol, stream)
					for _, payload := range []map[string]any{plain, sampled} {
						response := doJSON(t, handler, "POST", path, budgetRequestJSON(t, payload))
						if response.Code != 200 || !strings.Contains(response.Body.String(), "fixture text response") {
							t.Fatalf("%d: %s", response.Code, response.Body.String())
						}
						if protocol == "responses" {
							for _, object := range budgetResponseObjects(t, response.Body.String(), stream) {
								assertSamplingNull(t, object)
							}
						}
					}
					service.mu.Lock()
					defer service.mu.Unlock()
					if len(service.bodies) != 2 || len(service.requests) != 2 {
						t.Fatal("wrong submit count")
					}
					var wire [2][]any
					for index, request := range service.requests {
						c := request.Config
						if c.Temperature != nil || c.TopP != nil || c.TopK != nil || c.Seed != nil || c.MaxOutputTokens != nil {
							t.Fatal("uncleared config at physical submit")
						}
						if err := json.Unmarshal(service.bodies[index], &wire[index]); err != nil {
							t.Fatal(err)
						}
						if len(wire[index]) != 29 {
							t.Fatal("wire shape changed")
						}
						wire[index][11], wire[index][28] = nil, nil
					}
					if !reflect.DeepEqual(wire[0], wire[1]) {
						t.Fatal("sampling changed effective wire/body/system/thinking")
					}
				})
			}
		}
	}
}

func TestAssistantSamplingParsingAndAliases(t *testing.T) {
	cases := []struct {
		name, protocol string
		fields         map[string]any
		accepted       bool
	}{
		{"chat-null", "chat", map[string]any{"temperature": nil, "top_p": nil, "seed": nil}, true},
		{"chat-range", "chat", map[string]any{"temperature": 2, "top_p": 0, "seed": -1}, true},
		{"chat-temperature-type", "chat", map[string]any{"temperature": "0.8"}, false},
		{"chat-temperature-invalid", "chat", map[string]any{"temperature": 2.1}, false},
		{"chat-top-p-invalid", "chat", map[string]any{"top_p": -0.1}, false},
		{"chat-seed-float", "chat", map[string]any{"seed": json.RawMessage(`1.0`)}, false},
		{"chat-seed-overflow", "chat", map[string]any{"seed": json.RawMessage(`9223372036854775808`)}, false},
		{"chat-unmapped-top-k", "chat", map[string]any{"top_k": "not-supported"}, true},
		{"responses-range", "responses", map[string]any{"temperature": 0, "top_p": 1}, true},
		{"responses-mixed", "responses", map[string]any{"temperature": 0.8, "top_p": 1.1}, false},
		{"responses-unknown", "responses", map[string]any{"top_k": "not-supported", "seed": "not-supported"}, true},
		{"gemini-all-snake", "gemini", map[string]any{"generation_config": map[string]any{"temperature": 2, "top_p": 0, "top_k": 0, "seed": json.RawMessage(`9223372036854775807`)}}, true},
		{"gemini-camel-wins", "gemini", map[string]any{"generationConfig": map[string]any{"top_p": "discarded", "topP": 0.9, "top_k": -1, "topK": 0}}, true},
		{"gemini-invalid-camel-wins", "gemini", map[string]any{"generationConfig": map[string]any{"top_p": 0.9, "topP": 1.1}}, false},
		{"gemini-top-k-float", "gemini", map[string]any{"generationConfig": map[string]any{"topK": json.RawMessage(`1.0`)}}, false},
		{"gemini-top-k-overflow", "gemini", map[string]any{"generationConfig": map[string]any{"topK": json.RawMessage(`9223372036854775808`)}}, false},
		{"gemini-negative-top-k", "gemini", map[string]any{"generationConfig": map[string]any{"topK": -1}}, false},
		{"gemini-negative-overflow-seed", "gemini", map[string]any{"generationConfig": map[string]any{"seed": json.RawMessage(`-9223372036854775809`)}}, false},
		{"gemini-null", "gemini", map[string]any{"generationConfig": map[string]any{"temperature": nil, "topP": nil, "topK": nil, "seed": nil}}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service := newAssistantTextIntegration(t)
			handler := newRoutingTestHandler(t, service)
			path, payload := assistantBudgetPayload(t, "gemini-4-argon", test.protocol, false, nil)
			for key, value := range test.fields {
				payload[key] = value
			}
			result := doJSON(t, handler, "POST", path, budgetRequestJSON(t, payload))
			if (result.Code == 200) != test.accepted {
				t.Fatalf("HTTP=%d %s", result.Code, result.Body.String())
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if test.accepted {
				if len(service.requests) != 1 {
					t.Fatal("accepted input did not generate exactly once")
				}
			} else {
				if len(service.requests) != 0 {
					t.Fatal("invalid input generated")
				}
				for _, method := range service.auxMethods {
					if method != "ListModels" {
						t.Fatalf("invalid input acquired resource: %s", method)
					}
				}
			}
		})
	}
}

func TestAssistantSamplingResponseProjection(t *testing.T) {
	temp, probability, badTemp, badP := 0.8, 0.5, 2.1, 1.1
	for _, model := range []string{"assistant/gemini-4-argon", "models/assistant/gemini-3.8-flash", "playground/gemini-4-argon", "build/gemini-4-argon", "gemini-4-argon", "assistant/"} {
		for _, config := range []struct{ temperature, topP *float64 }{{}, {&temp, &probability}, {&temp, &badP}, {&badTemp, &probability}} {
			request := responsesRequest{Model: model, Temperature: config.temperature, TopP: config.topP, Metadata: map[string]string{"keep": "customer"}}
			for _, status := range []string{"queued", "in_progress", "completed", "failed", "cancelled"} {
				raw, err := json.Marshal(responseShell("fixture", 1, status, request))
				if err != nil {
					t.Fatal(err)
				}
				var shell map[string]any
				_ = json.Unmarshal(raw, &shell)
				for name, value := range map[string]*float64{"temperature": config.temperature, "top_p": config.topP} {
					var want any
					if value != nil {
						want = *value
						if strings.Contains(model, "assistant/") && model != "assistant/" && ((name == "temperature" && *value <= 2) || (name == "top_p" && *value <= 1)) {
							want = nil
						}
					}
					if shell[name] != want {
						t.Fatalf("model=%s status=%s %s got=%v want=%v", model, status, name, shell[name], want)
					}
				}
				if request.Temperature != config.temperature || request.TopP != config.topP || !strings.Contains(string(raw), `"keep":"customer"`) {
					t.Fatal("input or metadata changed")
				}
			}
		}
	}
}

func TestAssistantSamplingResponsesBackgroundStored(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"valid", "mixed_top_p", "mixed_temperature"} {
			t.Run(fmt.Sprintf("%t/%s", stream, mode), func(t *testing.T) {
				service := newAssistantTextIntegration(t)
				handler := newRoutingTestHandler(t, service)
				path, payload := assistantSamplingPayload(t, "gemini-4-argon", "responses", stream)
				payload["background"] = true
				if mode == "mixed_top_p" {
					payload["temperature"], payload["top_p"] = 0.8, 1.1
				}
				if mode == "mixed_temperature" {
					payload["temperature"], payload["top_p"] = 2.1, 0.5
				}
				result := doJSON(t, handler, "POST", path, budgetRequestJSON(t, payload))
				if result.Code != 200 {
					t.Fatalf("queued status retroactively rejected: %d %s", result.Code, result.Body.String())
				}
				objects := budgetResponseObjects(t, result.Body.String(), stream)
				id, _ := objects[0]["id"].(string)
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
				wantStatus := "completed"
				if mode != "valid" {
					wantStatus = "failed"
				}
				if final["status"] != wantStatus {
					t.Fatalf("bad final: %+v", final)
				}
				for _, object := range append(objects, final) {
					if mode == "valid" {
						assertSamplingNull(t, object)
					}
					if mode == "mixed_top_p" && (object["temperature"] != nil || object["top_p"] != 1.1) {
						t.Fatal("mixed projection hid invalid top_p")
					}
					if mode == "mixed_temperature" && (object["temperature"] != 2.1 || object["top_p"] != nil) {
						t.Fatal("mixed projection hid invalid temperature")
					}
				}
				service.mu.Lock()
				defer service.mu.Unlock()
				wantSends := 1
				if mode != "valid" {
					wantSends = 0
				}
				if len(service.requests) != wantSends {
					t.Fatal("background duplicated or invalid generated")
				}
			})
		}
	}
}

// Ensure the helper is only a local compatibility validator, not new global semantics.
func TestAssistantSamplingUnmappedFieldsRemainUnmapped(t *testing.T) {
	chat := chatRequest{Model: "assistant/gemini-4-argon"}
	if err := json.Unmarshal([]byte(`{"model":"assistant/gemini-4-argon","top_k":40}`), &chat); err != nil {
		t.Fatal(err)
	}
	request, err := chat.toGenerateRequest("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if request.Config.TopK != nil {
		t.Fatal("Chat gained top_k mapping")
	}
	var response responsesRequest
	if err := json.Unmarshal([]byte(`{"model":"assistant/gemini-4-argon","input":"fixture","top_k":40,"seed":1}`), &response); err != nil {
		t.Fatal(err)
	}
	generated, _, err := response.toGenerateRequest("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if generated.Config.TopK != nil || generated.Config.Seed != nil {
		t.Fatal("Responses gained unmapped fields")
	}
}

func TestAssistantSamplingForeignSignatureStillRefused(t *testing.T) {
	service := newAssistantTextIntegration(t)
	handler := newRoutingTestHandler(t, service)
	body := `{"generationConfig":{"temperature":0.8,"topP":0.9,"topK":40,"seed":0},"contents":[{"role":"model","parts":[{"thoughtSignature":"synthetic-unknown-signature","functionCall":{"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]}]}`
	result := doJSON(t, handler, "POST", "/v1beta/models/assistant/gemini-4-argon:generateContent", body)
	if result.Code != 400 || !strings.Contains(result.Body.String(), "cannot safely reuse upstream thought signature") {
		t.Fatalf("signature bypass: %d %s", result.Code, result.Body.String())
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.requests) != 0 {
		t.Fatal("foreign signature generated")
	}
}
