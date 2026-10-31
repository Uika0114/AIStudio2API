package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// Token counting and a pre-submit retry refusal are also deterministic fixtures.
// Real tracked Generate/outer retry, pool leases, Pooled/Client, codec/decoder,
// API middleware and physical-attempt observation execute; real worker
// promotion, browsers and account credentials are not exercised.
type assistantBudgetService struct {
	*aistudio.PooledService
	mu              sync.Mutex
	calls           int
	retryBeforeSend bool
}

func (s *assistantBudgetService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{InputTokens: 100000}, nil
}

func (s *assistantBudgetService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	s.mu.Lock()
	s.calls++
	firstRetry := s.retryBeforeSend && s.calls == 1
	s.mu.Unlock()
	if firstRetry {
		// A deliberately pre-send retryable refusal: not an accepted task replay.
		return nil, &aistudio.RPCError{Method: "fixture-before-submit", StatusCode: 429, Code: 8, Message: "fixture pre-send cooldown", RetryDelay: time.Hour}
	}
	return s.PooledService.Generate(ctx, request)
}

type assistantBudgetPreparer struct {
	state         aistudio.StorageState
	mu            sync.Mutex
	calls         int
	failAfterSend bool
	failReply     bool
	purposes      []string
}

func (p *assistantBudgetPreparer) Prepare(_ context.Context, request aistudio.ProtectedRequest) (aistudio.PreparedProtectedRequest, error) {
	return aistudio.PreparedProtectedRequest{Headers: request.Headers.Clone(), Body: append([]byte(nil), request.Body...)}, nil
}
func (p *assistantBudgetPreparer) BrowserStorageState(context.Context) (aistudio.StorageState, error) {
	return p.state, nil
}
func (p *assistantBudgetPreparer) SendProtected(ctx context.Context, _ aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	purpose := aistudio.GenerationPurpose(ctx)
	p.purposes = append(p.purposes, purpose)
	fail := p.failAfterSend || (p.failReply && purpose == "reply")
	p.mu.Unlock()
	if fail {
		return nil, io.ErrUnexpectedEOF
	}
	return budgetRPCResponse([]any{fmt.Sprintf("FIXTURE-GEN-%d", call), "FIXTURE-APPLET"}), nil
}

func budgetRPCResponse(value any) *aistudio.RPCResponse {
	data, _ := json.Marshal(value)
	return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(data))}
}

type assistantBudgetAdmin struct {
	api.AdminService
	mu     sync.Mutex
	finals []api.AccessLog
}

func (a *assistantBudgetAdmin) RecordAccessStart(api.AccessLog) {}
func (a *assistantBudgetAdmin) RecordAccessLog(value api.AccessLog) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finals = append(a.finals, value)
}

type assistantBudgetFixture struct {
	tracked  *trackedService
	backend  *assistantBudgetService
	preparer *assistantBudgetPreparer
	registry *requestRegistry
	pool     *aistudio.AccountPool
	admin    *assistantBudgetAdmin
	handler  http.Handler
	mu       sync.Mutex
	methods  []string
	requests []aistudio.GenerateRequest
	stream   func(context.Context) *aistudio.RPCResponse
}

func newAssistantBudgetFixture(t *testing.T, accountCount int) *assistantBudgetFixture {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "synthetic-state.json")
	if err := os.WriteFile(statePath, []byte(`{"cookies":[{"name":"SAPISID","value":"synthetic-not-a-credential","domain":".google.com","path":"/","expires":-1,"secure":true}],"origins":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := aistudio.LoadStorageState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookie := state.Cookies[0]
		cookie.Name = name
		state.Cookies = append(state.Cookies, cookie)
	}
	model := aistudio.Model{ID: "gemini-4-argon", Name: "fixture", Methods: []string{"generateContent", "countTokens"}, InputTokenLimit: 1048576, OutputTokenLimit: 32768, Capabilities: map[string]bool{"chat_model": true, "capability_code_48": true}, Channels: []string{"playground", "assistant"}}
	var accounts []*aistudio.Account
	for index := range accountCount {
		config := aistudio.DefaultAccountConfig(fmt.Sprintf("fixture-%d", index))
		config.Enabled = true
		accounts = append(accounts, &aistudio.Account{ID: fmt.Sprintf("fixture-budget-%d", index), Directory: dir, StoragePath: statePath, RuntimePath: filepath.Join(dir, fmt.Sprintf("runtime-%d.json", index)), StorageState: state, Config: config, State: aistudio.AccountReady, Models: []aistudio.Model{model}})
	}
	pool := aistudio.NewAccountPool(accounts, 1)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelAssistant})
	registry := newRequestRegistry(t.Context())
	fixture := &assistantBudgetFixture{registry: registry, pool: pool, admin: &assistantBudgetAdmin{}}
	preparer := &assistantBudgetPreparer{state: state}
	fixture.preparer = preparer
	plain, err := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
		return nil, errors.New("external network forbidden")
	})})
	if err != nil {
		t.Fatal(err)
	}
	protected, err := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{Transport: plain, Workers: aistudio.ProtectedPreparerProviderFunc(func(context.Context, string, string) (aistudio.ProtectedPreparer, error) { return preparer, nil }), SetupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := aistudio.NewClient(aistudio.ClientOptions{
		Transport: antiAuxiliaryRPC(func(ctx context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			fixture.mu.Lock()
			fixture.methods = append(fixture.methods, request.Method)
			fixture.mu.Unlock()
			switch request.Method {
			case "ListModels":
				row := make([]any, 83)
				row[0] = "models/gemini-4-argon"
				row[3] = "fixture"
				row[5] = 1048576
				row[6] = 32768
				row[7] = []any{"generateContent", "countTokens"}
				row[64] = []any{48}
				return budgetRPCResponse([]any{[]any{row}}), nil
			case "CreateApplet":
				return budgetRPCResponse([]any{"FIXTURE-APPLET", []any{"FIXTURE-APPLET"}}), nil
			case "StreamCodeAssistantOfflineGeneration":
				if fixture.stream != nil {
					return fixture.stream(ctx), nil
				}
				step := make([]any, 17)
				step[0] = []any{[]any{[]any{[]any{nil, "fixture complete text longer than budget one"}}, "model"}}
				step[11] = 1
				step[12] = 1
				step[13] = "models/gemini-4-argon"
				step[15] = []any{"10", "5", "2"}
				step[16] = "FIXTURE-GEN"
				return budgetRPCResponse([]any{[]any{[]any{nil, nil, []any{step}, "FIXTURE-APPLET"}}}), nil
			case "CancelCodeAssistantOfflineGeneration", "DeleteApplet":
				return budgetRPCResponse([]any{}), nil
			default:
				return nil, fmt.Errorf("unexpected offline RPC %s", request.Method)
			}
		}),
		Protected: aistudio.ProtectedTransportFunc(func(ctx context.Context, request aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
			if request.Config.MaxOutputTokens != nil || request.Selection.Channel != aistudio.ChannelAssistant {
				t.Error("wrong effective Assistant request")
			}
			fixture.mu.Lock()
			fixture.requests = append(fixture.requests, request)
			fixture.mu.Unlock()
			return protected.DoProtected(ctx, request, rpc)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if _, err := client.ModelsForAccount(t.Context(), account.ID); err != nil {
			t.Fatal(err)
		}
	}
	pooled, err := aistudio.NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	backend := &assistantBudgetService{PooledService: pooled}
	fixture.backend = backend
	workers := &accountWorkerManager{pool: pool, accounts: map[string]*accountWorker{}, lifecycle: t.Context(), requests: registry}
	tracked := newTrackedService(t.Context(), backend, pool, registry, workers, 3*time.Second)
	tracked.dataContext = t.Context()
	tracked.state.Store(serviceRunning)
	tracked.models = []aistudio.Model{model}
	tracked.proxies = aistudio.NewProxyPool(pool, nil)
	tracked.leaseAcquire = func(ctx context.Context, selection aistudio.AccountSelection) (*aistudio.AccountLease, error) {
		lease, _, err := pool.TryAcquireFor(ctx, selection)
		if lease == nil && err == nil {
			err = errors.New("fixture pool lease unavailable")
		}
		return lease, err
	}
	fixture.tracked = tracked
	fixture.handler = api.NewHandler(tracked, api.Config{Admin: fixture.admin})
	return fixture
}

func (f *assistantBudgetFixture) compatibilityLogs() []api.AdminLog {
	f.registry.mu.Lock()
	defer f.registry.mu.Unlock()
	var result []api.AdminLog
	for _, entry := range f.registry.logs {
		if entry.Event == "request.progress" && strings.HasPrefix(entry.Message, "assistant_output_budget_ignored ") {
			result = append(result, entry)
		}
	}
	return result
}

func budgetAppPost(t *testing.T, handler http.Handler, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAssistantOutputBudgetTrackedExecutionLogging(t *testing.T) {
	for _, mode := range []string{"positive", "omitted", "zero", "other_invalid", "pre_send_retry", "accepted_failure"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newAssistantBudgetFixture(t, 2)
			fixture.backend.retryBeforeSend = mode == "pre_send_retry"
			fixture.preparer.failAfterSend = mode == "accepted_failure"
			payload := map[string]any{"model": "assistant/gemini-4-argon", "messages": []any{map[string]any{"role": "user", "content": "private-input-sentinel"}}}
			if mode != "omitted" {
				payload["max_tokens"] = 1
			}
			if mode == "zero" {
				payload["max_tokens"] = 0
			}
			if mode == "other_invalid" {
				payload["temperature"] = 2.1 // Still invalid after compatibility cleanup.
			}
			result := budgetAppPost(t, fixture.handler, "/v1/chat/completions", payload)
			wantCompatible := mode != "zero" && mode != "omitted" && mode != "other_invalid"
			logs := fixture.compatibilityLogs()
			wantCount := 0
			if wantCompatible {
				wantCount = 1
			}
			if len(logs) != wantCount {
				t.Fatalf("compatibility log count=%d want=%d; HTTP=%d %s", len(logs), wantCount, result.Code, result.Body.String())
			}
			if wantCompatible && (!strings.Contains(logs[0].Message, "max_output_tokens=1 purpose=reply") || strings.Contains(logs[0].Message, "private-input-sentinel")) {
				t.Fatalf("wrong marker: %+v", logs[0])
			}
			if mode == "zero" || mode == "other_invalid" {
				if result.Code != 400 {
					t.Fatalf("invalid request: %d %s", result.Code, result.Body.String())
				}
				fixture.mu.Lock()
				sends := len(fixture.requests)
				fixture.mu.Unlock()
				if sends != 0 {
					t.Fatal("invalid input sent")
				}
			} else if mode == "accepted_failure" {
				if result.Code == 200 {
					t.Fatal("accepted but unreadable submit became success")
				}
				fixture.preparer.mu.Lock()
				calls := fixture.preparer.calls
				fixture.preparer.mu.Unlock()
				if calls != 1 {
					t.Fatalf("accepted failure replayed: %d", calls)
				}
			} else if result.Code != 200 {
				t.Fatalf("%d: %s", result.Code, result.Body.String())
			}
			fixture.backend.mu.Lock()
			calls := fixture.backend.calls
			fixture.backend.mu.Unlock()
			if mode == "pre_send_retry" && calls != 2 {
				t.Fatalf("fixture did not exercise outer retry: %d", calls)
			}
			fixture.admin.mu.Lock()
			finals := append([]api.AccessLog(nil), fixture.admin.finals...)
			fixture.admin.mu.Unlock()
			if len(finals) != 1 {
				t.Fatalf("root log count=%d", len(finals))
			}
			if mode != "omitted" && requestLogData(finals[0]).Parameters["max_output_tokens"] != fmt.Sprint(payload["max_tokens"]) {
				t.Fatal("raw input budget was rewritten in request log")
			}
			physical := map[string]bool{}
			for _, attempt := range finals[0].Attempts {
				if attempt.Kind != "generation" {
					continue
				}
				if physical[attempt.ID] {
					t.Fatal("duplicate physical attempt ID")
				}
				physical[attempt.ID] = true
				if attempt.Sent == nil || !*attempt.Sent {
					t.Fatal("physical submit was not marked sent")
				}
			}
			wantSends := 1
			if mode == "zero" || mode == "other_invalid" {
				wantSends = 0
			}
			if len(physical) != wantSends {
				t.Fatalf("physical submits=%d want=%d", len(physical), wantSends)
			}
			if wantSends == 0 {
				fixture.mu.Lock()
				for _, method := range fixture.methods {
					if method != "ListModels" {
						t.Errorf("invalid input caused resource RPC: %s", method)
					}
				}
				fixture.mu.Unlock()
			}
			if fixture.registry.count() != 0 {
				t.Fatal("tracked execution leaked")
			}
		})
	}
}

func TestAssistantOutputBudgetTrackedCompactionAndReplySameID(t *testing.T) {
	fixture := newAssistantBudgetFixture(t, 1)
	budget := int64(1)
	request := aistudio.GenerateRequest{ID: "same-generation-id", Model: "assistant/gemini-4-argon", Config: aistudio.GenerationConfig{MaxOutputTokens: &budget}, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "fixture"}}}}}
	for _, purpose := range []string{"compaction", "reply"} {
		ctx := aistudio.ContextWithGenerationPurpose(t.Context(), purpose)
		source, err := fixture.tracked.Generate(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		for event := range source {
			if event.Kind == aistudio.EventError {
				t.Fatal(event.Err)
			}
		}
	}
	logs := fixture.compatibilityLogs()
	if len(logs) != 2 || !strings.Contains(logs[0].Message, "purpose=compaction") || !strings.Contains(logs[1].Message, "purpose=reply") {
		t.Fatalf("execution purposes collapsed: %+v", logs)
	}
	if budget != 1 {
		t.Fatal("caller budget changed")
	}
}

func TestAssistantOutputBudgetHTTPAutoCompaction(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			fixture := newAssistantBudgetFixture(t, 1)
			payload := map[string]any{"model": "assistant/gemini-4-argon", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 9000)}, map[string]any{"role": "assistant", "content": strings.Repeat("past answer ", 9000)}, map[string]any{"role": "user", "content": "current"}}, "context_management": []any{map[string]any{"type": "compaction", "compact_threshold": 1}}}
			if explicit {
				payload["max_output_tokens"] = 1
			}
			result := budgetAppPost(t, fixture.handler, "/v1/responses", payload)
			if result.Code != 200 || !strings.Contains(result.Body.String(), `"type":"compaction"`) {
				t.Fatalf("auto compaction was not exercised: %d %s", result.Code, result.Body.String())
			}
			logs := fixture.compatibilityLogs()
			want := 0
			if explicit {
				want = 2
			}
			if len(logs) != want {
				t.Fatalf("compatibility executions=%d want=%d", len(logs), want)
			}
			fixture.preparer.mu.Lock()
			purposes := append([]string(nil), fixture.preparer.purposes...)
			fixture.preparer.mu.Unlock()
			if len(purposes) != 2 || purposes[0] != "compaction" || purposes[1] != "reply" {
				t.Fatalf("physical executions: %v", purposes)
			}
			fixture.admin.mu.Lock()
			finals := append([]api.AccessLog(nil), fixture.admin.finals...)
			fixture.admin.mu.Unlock()
			if len(finals) != 1 {
				t.Fatalf("root request count=%d", len(finals))
			}
			ids := map[string]bool{}
			for _, attempt := range finals[0].Attempts {
				if attempt.Kind == "generation" {
					if ids[attempt.ID] {
						t.Fatal("duplicate physical attempt")
					}
					ids[attempt.ID] = true
				}
			}
			if len(ids) != 2 {
				t.Fatalf("unique physical attempts=%d", len(ids))
			}
		})
	}
}
