package api

import (
	"strings"
	"testing"
)

func TestAssistantOutputBudgetNoGenerationBoundaries(t *testing.T) {
	for _, mode := range []string{"no_history_compact", "malformed_json", "signature_provenance"} {
		t.Run(mode, func(t *testing.T) {
			service := newAssistantTextIntegration(t)
			handler := newRoutingTestHandler(t, service)
			path, body := "/v1/responses/compact", `{"model":"assistant/gemini-4-argon","max_output_tokens":0,"input":"short"}`
			want := 200
			if mode == "malformed_json" {
				path, body, want = "/v1/chat/completions", `{"model":"assistant/gemini-4-argon","max_tokens":1,`, 400
			}
			if mode == "signature_provenance" {
				path, body, want = "/v1beta/models/assistant/gemini-4-argon:generateContent", `{"generationConfig":{"maxOutputTokens":1},"contents":[{"role":"model","parts":[{"thoughtSignature":"synthetic-unknown-signature","functionCall":{"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]}]}`, 400
			}
			result := doJSON(t, handler, "POST", path, body)
			if result.Code != want {
				t.Fatalf("%s HTTP=%d want=%d %s", mode, result.Code, want, result.Body.String())
			}
			if mode == "signature_provenance" && !strings.Contains(result.Body.String(), "cannot safely reuse upstream thought signature") {
				t.Fatalf("did not exercise provenance: %s", result.Body.String())
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if len(service.requests) != 0 {
				t.Fatal("non-generation operation submitted")
			}
			for _, method := range service.auxMethods {
				if method != "ListModels" {
					t.Fatalf("unexpected resource RPC: %s", method)
				}
			}
		})
	}
}
