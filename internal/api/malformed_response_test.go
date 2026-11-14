package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestMalformedResponseFourProtocolTerminals(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				service := &fakeService{models: []aistudio.Model{routingTestModel()}, generateEvents: []aistudio.Event{
					{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 10, TotalTokens: 10}},
					{Kind: aistudio.EventFinish, FinishReason: "malformed_response"},
				}}
				path, payload, expected := "", "", ""
				switch protocol {
				case "chat":
					path = "/v1/chat/completions"
					payload = fmt.Sprintf(`{"model":"playground/gemini-4-argon","messages":[{"role":"user","content":"fixture"}],"stream":%v}`, stream)
					expected = `"finish_reason":"stop"`
				case "responses":
					path = "/v1/responses"
					payload = fmt.Sprintf(`{"model":"playground/gemini-4-argon","input":"fixture","stream":%v}`, stream)
					expected = `"status":"completed"`
				case "anthropic":
					path = "/v1/messages"
					payload = fmt.Sprintf(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"fixture"}],"max_tokens":1024,"stream":%v}`, stream)
					expected = `"stop_reason":"end_turn"`
				case "gemini":
					path = "/v1beta/models/playground/gemini-4-argon:generateContent"
					if stream {
						path = "/v1beta/models/playground/gemini-4-argon:streamGenerateContent?alt=sse"
					}
					payload = `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
					expected = `"finishReason":"MALFORMED_RESPONSE"`
				}
				response := doJSON(t, newRoutingTestHandler(t, service), http.MethodPost, path, payload)
				body := response.Body.String()
				if response.Code != 200 || !strings.Contains(body, expected) || strings.Contains(body, "content_filter") || strings.Contains(body, "refusal") || strings.Contains(body, "provider_19") {
					t.Fatalf("wrong terminal: status=%d body=%s", response.Code, body)
				}
				if service.generateCount() != 1 {
					t.Fatal("adapter added its own replay")
				}
			})
		}
	}
}

func TestMalformedResponseDoesNotWeakenSafetyOrToolMapping(t *testing.T) {
	for _, reason := range []string{"safety", "recitation", "blocklist", "prohibited_content", "spii", "image_safety", "image_prohibited_content", "image_recitation", "provider_99"} {
		for _, tools := range []bool{false, true} {
			if openAIFinishReason(reason, tools) != "content_filter" {
				t.Fatalf("safety mapping changed: %s", reason)
			}
			if stop, _ := anthropicStop(reason, tools, ""); stop != "refusal" {
				t.Fatalf("safety mapping changed: %s", reason)
			}
		}
	}
	if openAIFinishReason("malformed_response", true) != "tool_calls" {
		t.Fatal("tool finish lost")
	}
	if stop, _ := anthropicStop("malformed_response", true, ""); stop != "tool_use" {
		t.Fatal("tool finish lost")
	}
}
