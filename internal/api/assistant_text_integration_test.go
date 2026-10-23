package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// These tests use the real pooled/client pipeline below all three adapters.
// Only the auxiliary/protected RPC transports are synthetic; no HTTP is sent.
type assistantTextIntegrationService struct {
	*fakeService
	pooled *aistudio.PooledService
	lease  *aistudio.AccountLease
	mu     sync.Mutex
	bodies [][]byte
}

func (s *assistantTextIntegrationService) Generate(ctx context.Context, r aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if err := s.pooled.ValidateGenerateRequest(r); err != nil {
		return nil, err
	}
	return s.pooled.Generate(aistudio.ContextWithAccountLease(ctx, s.lease), r)
}
func newAssistantTextIntegration(t *testing.T, streamBody ...func(context.Context, []byte) io.ReadCloser) *assistantTextIntegrationService {
	t.Helper()
	model := routingTestModel()
	model.Capabilities["capability_code_48"] = true
	account := &aistudio.Account{ID: "fixture-text", Config: aistudio.AccountConfig{Enabled: true}, State: aistudio.AccountReady, Models: []aistudio.Model{model}}
	pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild, aistudio.ChannelAssistant})
	lease, _, err := pool.TryAcquireFor(context.Background(), aistudio.AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: aistudio.ChannelAssistant, StrictChannel: true})
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	})
	service := &assistantTextIntegrationService{fakeService: &fakeService{models: []aistudio.Model{assistantRoutingModel()}}, lease: lease}
	response := func(v any) *aistudio.RPCResponse {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(body))}
	}
	transport := assistantTextAuxFunc(func(ctx context.Context, r aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		switch r.Method {
		case "ListModels":
			row := make([]any, 83)
			row[0] = "models/gemini-4-argon"
			row[3] = "fixture model"
			row[5] = 1048576
			row[6] = 32768
			row[7] = []any{"generateContent"}
			row[64] = []any{48}
			return response([]any{[]any{row}}), nil
		case "CreateApplet":
			return response([]any{"FIXTURE-APPLET", []any{"FIXTURE-APPLET"}}), nil
		case "StreamCodeAssistantOfflineGeneration":
			part := []any{nil, "fixture text response"}
			step := make([]any, 17)
			step[0] = []any{[]any{[]any{part}, "model"}}
			step[11] = 1
			step[12] = 1
			step[13] = "models/gemini-4-argon"
			step[15] = []any{"10", "5", "2"}
			step[16] = "FIXTURE-GEN"
			result := response([]any{[]any{[]any{nil, nil, []any{step}, "FIXTURE-APPLET"}}})
			if len(streamBody) != 0 {
				wire, err := io.ReadAll(result.Body)
				_ = result.Body.Close()
				if err != nil {
					return nil, err
				}
				result.Body = streamBody[0](ctx, wire)
			}
			return result, nil
		case "CancelCodeAssistantOfflineGeneration", "DeleteApplet":
			return response([]any{}), nil
		default:
			return nil, fmt.Errorf("unexpected offline auxiliary RPC: %s", r.Method)
		}
	})
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: transport, Protected: aistudio.ProtectedTransportFunc(func(_ context.Context, r aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		if rpc.Method != "CodeAssistantOffline" || r.Selection.Channel != aistudio.ChannelAssistant || len(r.Tools.Functions) != 0 || r.Tools.ToolConfig.Mode != "" {
			t.Errorf("wrong normalized submit: %s %+v", rpc.Method, r.Tools)
		}
		service.mu.Lock()
		service.bodies = append(service.bodies, append([]byte(nil), rpc.Body...))
		service.mu.Unlock()
		return response([]any{"FIXTURE-GEN", "FIXTURE-APPLET"}), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ModelsForAccount(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	service.pooled, err = aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type assistantTextAuxFunc func(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error)

func (f assistantTextAuxFunc) Do(ctx context.Context, r aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return f(ctx, r)
}

func TestAssistantTextDowngradeThreeProtocolsRealClient(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				service := newAssistantTextIntegration(t)
				handler := newRoutingTestHandler(t, service)
				var path, body string
				switch protocol {
				case "chat":
					path = "/v1/chat/completions"
					body = fmt.Sprintf(`{"model":"assistant/gemini-4-argon","stream":%t,"messages":[{"role":"user","content":"fixture"},{"role":"assistant","tool_calls":[{"id":"call-fixture","type":"function","function":{"name":"read_fixture","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-fixture","content":"fixture result"}],"tools":[{"type":"function","function":{"name":"read_fixture","parameters":{"type":"object"}}}],"tool_choice":"required","parallel_tool_calls":false}`, stream)
				case "responses":
					path = "/v1/responses"
					body = fmt.Sprintf(`{"model":"assistant/gemini-4-argon","stream":%t,"input":[{"role":"user","content":"fixture"},{"type":"function_call","call_id":"call-fixture","name":"read_fixture","arguments":"{}"},{"type":"function_call_output","call_id":"call-fixture","output":"fixture result"}],"tools":[{"type":"function","name":"read_fixture","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"read_fixture"},"parallel_tool_calls":false}`, stream)
				case "gemini":
					action := "generateContent"
					if stream {
						action = "streamGenerateContent?alt=sse"
					}
					path = "/v1beta/models/assistant/gemini-4-argon:" + action
					body = `{"contents":[{"role":"user","parts":[{"text":"fixture"}]},{"role":"model","parts":[{"functionCall":{"name":"read_fixture","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"read_fixture","response":{"result":"fixture result"}}}]}],"tools":[{"functionDeclarations":[{"name":"read_fixture","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["read_fixture"]}}}`
				}
				result := doJSON(t, handler, "POST", path, body)
				if result.Code != 200 || !strings.Contains(result.Body.String(), "fixture text response") {
					t.Fatalf("%d: %s", result.Code, result.Body.String())
				}
				if strings.Contains(result.Body.String(), `"tool_calls"`) || strings.Contains(result.Body.String(), `"function_call"`) || strings.Contains(result.Body.String(), `"functionCall"`) {
					t.Fatalf("fake native tools leaked: %s", result.Body.String())
				}
				service.mu.Lock()
				defer service.mu.Unlock()
				if len(service.bodies) != 1 {
					t.Fatalf("sends=%d", len(service.bodies))
				}
				var wire []json.RawMessage
				if err := json.Unmarshal(service.bodies[0], &wire); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(wire[0]), "functionResponse") || !strings.Contains(string(wire[0]), "read_fixture") {
					t.Fatalf("history transcript missing: %s", wire[0])
				}
				var turns []json.RawMessage
				_ = json.Unmarshal(wire[0], &turns)
				for _, raw := range turns {
					var turn []json.RawMessage
					_ = json.Unmarshal(raw, &turn)
					var parts [][]json.RawMessage
					_ = json.Unmarshal(turn[0], &parts)
					for _, part := range parts {
						if len(part) > 10 && string(part[10]) != "null" {
							t.Fatal("native history escaped")
						}
						if len(part) > 11 && string(part[11]) != "null" {
							t.Fatal("native result escaped")
						}
					}
				}
			})
		}
	}
}

func TestAssistantTextDowngradeResponsesBackgroundAndHistoryRealClient(t *testing.T) {
	service := newAssistantTextIntegration(t)
	handler := newRoutingTestHandler(t, service)
	first := doJSON(t, handler, "POST", "/v1/responses", `{"model":"assistant/gemini-4-argon","input":"fixture","store":true,"tools":[{"type":"function","name":"read_fixture","parameters":{"type":"object"}}],"tool_choice":"required"}`)
	if first.Code != 200 {
		t.Fatalf("%d: %s", first.Code, first.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	id, _ := saved["id"].(string)
	if id == "" {
		t.Fatal("missing stored response")
	}
	next := doJSON(t, handler, "POST", "/v1/responses", fmt.Sprintf(`{"previous_response_id":%q,"input":"fixture next","background":true,"stream":true}`, id))
	if next.Code != 200 || !strings.Contains(next.Body.String(), "response.completed") || !strings.Contains(next.Body.String(), "fixture text response") {
		t.Fatalf("%d: %s", next.Code, next.Body.String())
	}
	service.mu.Lock()
	sends := len(service.bodies)
	service.mu.Unlock()
	if sends != 2 {
		t.Fatalf("background sends=%d", sends)
	}
	server := &server{service: service, thoughtSignatures: newThoughtSignatureStore()}
	if _, _, err := server.compactionSummary(context.Background(), responsesRequest{Model: "assistant/gemini-4-argon"}, []json.RawMessage{json.RawMessage(`{"role":"user","content":"fixture summary"}`)}, "", "fixture-compaction"); err != nil {
		t.Fatal(err)
	}
	doJSON(t, handler, "GET", "/v1/responses/"+id, "")
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.bodies) != 3 {
		t.Fatal("retrieval generated or compaction failed to generate")
	}
}
