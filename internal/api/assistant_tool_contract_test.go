package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Adapters preserve the submitted contract until the core Assistant-only
// downgrade boundary. This injected error tests propagation, not the current
// production text policy; real-client coverage is in assistant_text_integration_test.go.
func TestAssistantToolContractAdaptersPreserveChoiceAndSchema(t *testing.T) {
	const schema = `{"type":"object","properties":{"path":{"type":"string","enum":["safe.txt"]}},"required":["path"],"additionalProperties":false}`
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, choice := range []string{"auto", "none", "required", "named"} {
			t.Run(protocol+"/"+choice, func(t *testing.T) {
				service := &fakeService{
					models:      []aistudio.Model{assistantRoutingModel()},
					generateErr: fmt.Errorf("%w: fixture core rejection", aistudio.ErrInvalidArgument),
				}
				handler := newRoutingTestHandler(t, service)
				var path, body, toolChoice string
				toolChoice = fmt.Sprintf("%q", choice)
				switch protocol {
				case "chat":
					path = "/v1/chat/completions"
					if choice == "named" {
						toolChoice = `{"type":"function","function":{"name":"read_fixture"}}`
					}
					body = fmt.Sprintf(`{"model":"assistant/gemini-4-argon","messages":[{"role":"user","content":"fixture"}],"tools":[{"type":"function","function":{"name":"read_fixture","description":"exact fixture schema","parameters":%s}}],"tool_choice":%s,"parallel_tool_calls":false}`, schema, toolChoice)
				case "responses":
					path = "/v1/responses"
					if choice == "named" {
						toolChoice = `{"type":"function","name":"read_fixture"}`
					}
					body = fmt.Sprintf(`{"model":"assistant/gemini-4-argon","input":"fixture","tools":[{"type":"function","name":"read_fixture","description":"exact fixture schema","parameters":%s}],"tool_choice":%s,"parallel_tool_calls":false}`, schema, toolChoice)
				case "gemini":
					path = "/v1beta/models/assistant/gemini-4-argon:generateContent"
					mode := map[string]string{"auto": "AUTO", "none": "NONE", "required": "ANY", "named": "ANY"}[choice]
					names := ""
					if choice == "named" {
						names = `,"allowedFunctionNames":["read_fixture"]`
					}
					body = fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}],"tools":[{"functionDeclarations":[{"name":"read_fixture","description":"exact fixture schema","parameters":%s}]}],"toolConfig":{"functionCallingConfig":{"mode":%q%s}}}`, schema, mode, names)
				}
				response := doJSON(t, handler, "POST", path, body)
				if response.Code != 400 || !strings.Contains(response.Body.String(), "fixture core rejection") || service.generateCount() != 1 {
					t.Fatalf("injected core failure lost: status=%d sends=%d body=%s", response.Code, service.generateCount(), response.Body.String())
				}
				request := service.lastGenerate(t)
				if request.Selection.Channel != aistudio.ChannelAssistant || request.Model != "gemini-4-argon" {
					t.Fatalf("route changed: %+v", request.Selection)
				}
				if len(request.Tools.Functions) != 1 || request.Tools.Functions[0].Name != "read_fixture" || request.Tools.Functions[0].Description != "exact fixture schema" {
					t.Fatalf("declaration silently stripped: %+v", request.Tools)
				}
				var want, got any
				if err := json.Unmarshal([]byte(schema), &want); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(request.Tools.Functions[0].Parameters, &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("schema was weakened: err=%v got=%s", err, request.Tools.Functions[0].Parameters)
				}
				wantMode := choice
				if choice == "named" {
					wantMode = "required"
					if !reflect.DeepEqual(request.Tools.ToolConfig.AllowedFunctionNames, []string{"read_fixture"}) {
						t.Fatalf("named choice lost: %+v", request.Tools.ToolConfig)
					}
				}
				if request.Tools.ToolConfig.Mode != wantMode {
					t.Fatalf("tool choice weakened: %+v", request.Tools.ToolConfig)
				}
				if protocol != "gemini" && (request.Tools.ToolConfig.ParallelCalls == nil || *request.Tools.ToolConfig.ParallelCalls) {
					t.Fatalf("parallel=false was dropped: %+v", request.Tools.ToolConfig)
				}
			})
		}
	}
}
