package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// Real API handlers, tracked retry logic, pooled service, pool leases and codecs
// run against synthetic in-memory accounts. No HTTP client, browser or WAA
// worker is created; both outbound transport boundaries are deterministic fakes.
type disabledHTTPFixture struct {
	handler   http.Handler
	protected atomic.Int64
	auxiliary atomic.Int64
}

func newDisabledHTTPFixture(t *testing.T) *disabledHTTPFixture {
	t.Helper()
	f := &disabledHTTPFixture{}
	model := aistudio.Model{ID: "gemini-4-argon", Name: "fixture", Methods: []string{"generateContent", "countTokens"}, InputTokenLimit: 1048576, OutputTokenLimit: 32768, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true, "capability_code_48": true}}
	account := &aistudio.Account{ID: "fixture-channel-disabled", Config: aistudio.AccountConfig{Enabled: true}, State: aistudio.AccountReady, Models: []aistudio.Model{model}}
	pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
	pool.SetUpstreamChannels(upstreamChannels(config.Default().UpstreamChannels))
	catalog := []aistudio.Model{model,
		{ID: "fixture-build-only", Methods: []string{"generateContent"}},
		{ID: "fixture-embedding", Methods: []string{"embedContent"}},
		{ID: "fixture-live", Methods: []string{"bidiGenerateContent"}},
		{ID: "fixture-music", Methods: []string{"bidiGenerateMusic"}},
	}
	if err := pool.SetBuildCatalog(account.ID, catalog); err != nil {
		t.Fatal(err)
	}
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport: antiAuxiliaryRPC(func(_ context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			f.auxiliary.Add(1)
			if request.Method != "ListModels" {
				return nil, fmt.Errorf("unexpected offline RPC %s", request.Method)
			}
			row := make([]any, 65)
			row[0], row[3], row[5], row[6] = "models/gemini-4-argon", "fixture", 1048576, 32768
			row[7], row[64] = []string{"generateContent", "countTokens"}, []int{1, 10, 48}
			return budgetRPCResponse([]any{[]any{row}}), nil
		}),
		Protected: aistudio.ProtectedTransportFunc(func(ctx context.Context, request aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			f.protected.Add(1)
			lease, ok := aistudio.AccountLeaseFromContext(ctx)
			if !ok || lease.Channel() != aistudio.ChannelPlayground || rpc.Method != "GenerateContent" {
				return nil, errors.New("disabled upstream reached protected transport")
			}
			part := aistudio.Part{Text: "fixture channel response"}
			if request.Selection.AntiTruncation {
				name := request.Tools.Functions[len(request.Tools.Functions)-1].Name
				part = aistudio.Part{FunctionCall: &aistudio.FunctionCall{ID: "fixture-internal", Name: name, Arguments: json.RawMessage(`{"content":"fixture channel response"}`)}}
			}
			// Use the public PG encoder for the synthetic model content shape.
			wire, err := aistudio.EncodeGenerateContentRequest(aistudio.GenerateRequest{Model: model.ID, Contents: []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{part}}}}, aistudio.GenerationDefaults{MaxOutputTokens: 32768}, aistudio.RequestContext{})
			if err != nil {
				return nil, err
			}
			var encoded []any
			if err := json.Unmarshal(wire, &encoded); err != nil {
				return nil, err
			}
			content := encoded[1].([]any)[0]
			candidate := []any{content, 1}
			return budgetRPCResponse([]any{[]any{[]any{[]any{candidate}, nil, []any{5, 3, 8}}}}), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ModelsForAccount(t.Context(), account.ID); err != nil {
		t.Fatal(err)
	}
	f.auxiliary.Store(0)
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	pooled.BuildNativeNonstream = true // This preference must not reopen Build.
	registry := newRequestRegistry(t.Context())
	workers := &accountWorkerManager{pool: pool, accounts: map[string]*accountWorker{}, lifecycle: t.Context(), requests: registry}
	tracked := newTrackedService(t.Context(), pooled, pool, registry, workers, 3*time.Second)
	tracked.dataContext = t.Context()
	tracked.state.Store(serviceRunning)
	tracked.models = catalog
	tracked.buildNativeNonstream = true
	tracked.proxies = aistudio.NewProxyPool(pool, nil)
	tracked.leaseAcquire = func(ctx context.Context, selection aistudio.AccountSelection) (*aistudio.AccountLease, error) {
		lease, _, err := pool.TryAcquireFor(ctx, selection)
		if lease == nil && err == nil {
			err = errors.New("fixture pool lease unavailable")
		}
		return lease, err
	}
	f.handler = api.NewHandler(tracked, api.Config{})
	return f
}

func disabledHTTPPost(t *testing.T, handler http.Handler, protocol, model string, stream, background bool) *httptest.ResponseRecorder {
	t.Helper()
	var path string
	var payload any
	switch protocol {
	case "chat":
		path = "/v1/chat/completions"
		payload = map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "fixture question"}}}
	case "responses":
		path = "/v1/responses"
		payload = map[string]any{"model": model, "stream": stream, "background": background, "input": "fixture question"}
	case "gemini":
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		path = "/v1beta/models/" + model + ":" + action
		payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "fixture question"}}}}}
	case "anthropic":
		path = "/v1/messages"
		payload = map[string]any{"model": model, "stream": stream, "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "fixture question"}}}
	default:
		t.Fatalf("unknown protocol %s", protocol)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestDisabledChannelsHTTPMatrix(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, prefix := range []string{"build/", "build/anti-truncation/", "assistant/", "playground/", "playground/anti-truncation/", ""} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", protocol, prefix, stream), func(t *testing.T) {
					f := newDisabledHTTPFixture(t)
					response := disabledHTTPPost(t, f.handler, protocol, prefix+"gemini-4-argon", stream, false)
					disabled := strings.HasPrefix(prefix, "build/") || prefix == "assistant/"
					if disabled {
						if f.protected.Load() != 0 || f.auxiliary.Load() != 0 {
							t.Fatalf("disabled request sent upstream: protected=%d auxiliary=%d", f.protected.Load(), f.auxiliary.Load())
						}
						if response.Code < 400 && !strings.Contains(response.Body.String(), "error") {
							t.Fatalf("disabled request did not fail: %d %s", response.Code, response.Body.String())
						}
						if strings.Contains(response.Body.String(), "fixture channel response") {
							t.Fatal("disabled request borrowed PG")
						}
					} else {
						if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "fixture channel response") {
							t.Fatalf("PG positive: %d %s", response.Code, response.Body.String())
						}
						if f.protected.Load() != 1 || f.auxiliary.Load() != 0 {
							t.Fatalf("PG sent count protected=%d auxiliary=%d", f.protected.Load(), f.auxiliary.Load())
						}
					}
				})
			}
		}
	}
}

func TestDisabledChannelsResponsesBackground(t *testing.T) {
	for _, prefix := range []string{"build/", "build/anti-truncation/", "assistant/", "playground/", "playground/anti-truncation/"} {
		t.Run(prefix, func(t *testing.T) {
			f := newDisabledHTTPFixture(t)
			response := disabledHTTPPost(t, f.handler, "responses", prefix+"gemini-4-argon", false, true)
			var state struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
			disabled := !strings.HasPrefix(prefix, "playground/")
			if response.Code >= 400 {
				if !disabled {
					t.Fatalf("PG background rejected: %s", response.Body.String())
				}
			} else {
				if state.ID == "" {
					t.Fatalf("missing background ID: %s", response.Body.String())
				}
				deadline := time.Now().Add(2 * time.Second)
				for state.Status == "queued" || state.Status == "in_progress" {
					if time.Now().After(deadline) {
						t.Fatal("background did not finish")
					}
					time.Sleep(time.Millisecond)
					response = httptest.NewRecorder()
					f.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/responses/"+state.ID, nil))
					if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
						t.Fatal(err)
					}
				}
				want := "completed"
				if disabled {
					want = "failed"
				}
				if state.Status != want {
					t.Fatalf("background status=%q want %q: %s", state.Status, want, response.Body.String())
				}
			}
			want := int64(1)
			if disabled {
				want = 0
			}
			if f.protected.Load() != want || f.auxiliary.Load() != 0 {
				t.Fatalf("background sent protected=%d auxiliary=%d", f.protected.Load(), f.auxiliary.Load())
			}
		})
	}
}

func TestDisabledChannelsAnthropicAutoAndCatalog(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("anthropic/stream=%t", stream), func(t *testing.T) {
			f := newDisabledHTTPFixture(t)
			response := disabledHTTPPost(t, f.handler, "anthropic", "gemini-4-argon", stream, false)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "fixture channel response") || f.protected.Load() != 1 || f.auxiliary.Load() != 0 {
				t.Fatalf("Anthropic auto did not stay on PG: %d %s", response.Code, response.Body.String())
			}
		})
	}
	for _, path := range []string{"/v1/models", "/v1beta/models"} {
		t.Run(path, func(t *testing.T) {
			f := newDisabledHTTPFixture(t)
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("catalog: %d %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, forbidden := range []string{"build/", "assistant/", "fixture-build-only", "fixture-embedding", "fixture-live", "fixture-music"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("disabled catalog entry %q: %s", forbidden, body)
				}
			}
			if !strings.Contains(body, "playground/gemini-4-argon") {
				t.Fatalf("missing PG variant: %s", body)
			}
			if f.protected.Load() != 0 || f.auxiliary.Load() != 0 {
				t.Fatal("catalog unexpectedly sent upstream")
			}
		})
	}
}

func TestUpstreamChannelSaveRequiresGenerationRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.env")
	active := config.Default()
	active.UpstreamChannels = []string{"playground", "build", "assistant"}
	if err := active.Save(path); err != nil {
		t.Fatal(err)
	}
	model := aistudio.Model{ID: "gemini-4-argon", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "capability_code_48": true}}
	newPool := func(cfg config.Config) *aistudio.AccountPool {
		account := &aistudio.Account{ID: "fixture-save", Config: aistudio.AccountConfig{Enabled: true}, State: aistudio.AccountReady, Models: []aistudio.Model{model}}
		pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
		pool.SetUpstreamChannels(upstreamChannels(cfg.UpstreamChannels))
		if err := pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
			t.Fatal(err)
		}
		return pool
	}
	pool := newPool(active)
	admin := &runtimeAdmin{configPath: path, config: active, pool: pool, requests: newRequestRegistry(t.Context())}
	value, err := admin.RuntimeConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	value.UpstreamChannels = []string{"playground"}
	updated, err := admin.UpdateRuntimeConfig(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	manager := &runtimeManager{activeManagement: active}
	decorated := manager.decorateRuntimeConfig(updated, active)
	if !decorated.ServiceRestartRequired || decorated.ManagementRestartRequired {
		t.Fatal("wrong restart flags")
	}
	saved, err := config.Load(path)
	if err != nil || !reflect.DeepEqual(saved.UpstreamChannels, []string{"playground"}) {
		t.Fatalf("saved: %v %v", saved.UpstreamChannels, err)
	}
	nextPool := newPool(saved) // Same config-to-pool wiring as the next runtime generation.
	for _, channel := range []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelAssistant} {
		selection := aistudio.AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: channel, StrictChannel: true}
		lease, _, err := pool.TryAcquireFor(t.Context(), selection)
		if err != nil || lease == nil {
			t.Fatalf("save unexpectedly hot-disabled %s: %v", channel, err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
		lease, _, _ = nextPool.TryAcquireFor(t.Context(), selection)
		if lease != nil {
			_ = lease.Release()
			t.Fatalf("next generation still allows %s", channel)
		}
	}
	if !sameDataConfig(updated, saved, dataConfigOverrides{}) {
		t.Fatal("new generation remains pending")
	}
}
