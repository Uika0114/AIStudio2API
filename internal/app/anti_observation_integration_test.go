package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
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
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

var antiEvidenceDirectory = flag.String("anti-evidence-dir", "", "optional destination for body-free synthetic ledger evidence")

type antiAuxiliaryRPC func(context.Context, aistudio.RPCRequest) (*aistudio.RPCResponse, error)

func (f antiAuxiliaryRPC) Do(ctx context.Context, r aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return f(ctx, r)
}

// These fixtures execute real Client decoding/projection, trackedService observer
// and forwardEvents, HTTP middleware, the asynchronous writer and cold CSV read.
// Scheduling, browser and external transports are not started.
type antiLedgerService struct {
	*aistudio.Client
	pool    *aistudio.AccountPool
	model   aistudio.Model
	account string
}

func (s *antiLedgerService) Models(context.Context) ([]aistudio.Model, error) {
	return []aistudio.Model{s.model}, nil
}
func (s *antiLedgerService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{InputTokens: 100}, nil
}
func (s *antiLedgerService) Generate(ctx context.Context, r aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	api.SetAccessLogModelSelection(ctx, r.Selection)
	ctx = observeGenerationMetadata(ctx, map[string]string{s.account: "fixture"})
	lease, _, err := s.pool.TryAcquireFor(ctx, aistudio.AccountSelection{ModelID: r.Model, Method: "generateContent", Channel: r.Selection.Channel, StrictChannel: true})
	if err != nil || lease == nil {
		return nil, errors.New("fixture lease unavailable")
	}
	r.AccountID = s.account
	source, err := s.Client.Generate(aistudio.ContextWithAccountLease(ctx, lease), r)
	if err != nil {
		lease.Release()
		return nil, err
	}
	first, err := firstGenerateEvent(ctx, source, func() {})
	if err != nil {
		lease.Release()
		return nil, err
	}
	tracked := &trackedService{pool: s.pool, requests: newRequestRegistry(ctx), lifecycle: ctx}
	tracked.requests.start(r, func() {})
	out := make(chan aistudio.Event)
	go tracked.forwardEvents(ctx, ctx, func() {}, r.ID, first, source, out, lease, nil, &upstreamActivity{}, r.Model)
	return out, nil
}

type antiLedgerPreparer struct {
	state                      aistudio.StorageState
	mu                         sync.Mutex
	name                       string
	build, unary               bool
	calls                      int
	fallback, compaction       bool
	unknownFinish, otherOutput bool
	authReplay                 bool
}

func (p *antiLedgerPreparer) Prepare(_ context.Context, r aistudio.ProtectedRequest) (aistudio.PreparedProtectedRequest, error) {
	return aistudio.PreparedProtectedRequest{Headers: r.Headers.Clone(), Body: append([]byte(nil), r.Body...)}, nil
}
func (p *antiLedgerPreparer) BrowserStorageState(context.Context) (aistudio.StorageState, error) {
	return p.state, nil
}
func (p *antiLedgerPreparer) SendProtected(context.Context, aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.authReplay && p.calls == 1 {
		return &aistudio.RPCResponse{StatusCode: 401, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader(`[16,"fixture auth expired"]`))}, nil
	}
	fallback := p.fallback || p.compaction && p.calls > 1
	finish := 19
	buildFinish := "SAFETY"
	if p.compaction {
		finish = 1
		buildFinish = "STOP"
	}
	if p.unknownFinish {
		buildFinish = "SYNTHETIC_PRIVATE_MARKER"
	}
	var root any
	if p.build {
		part := map[string]any{"functionCall": map[string]any{"name": p.name, "args": map[string]any{"content": "visible-fixture"}}}
		if fallback {
			part = map[string]any{"text": "visible-fixture"}
		}
		if p.otherOutput {
			part = map[string]any{"executableCode": map[string]any{"language": "PYTHON", "code": "print(1)"}}
		}
		inner, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}, "finishReason": buildFinish}}})
		if p.unary {
			root = []any{string(inner)}
		} else {
			root = []any{[]any{[]any{string(inner)}}}
		}
	} else {
		var part any = []any{nil, "visible-fixture"}
		if !fallback {
			fields := make([]any, 11)
			fields[10] = []any{p.name, []any{[]any{[]any{"content", []any{nil, nil, "visible-fixture"}}}}}
			part = fields
		}
		candidate := []any{[]any{[]any{part}, "model"}, finish}
		root = []any{[]any{[]any{[]any{candidate}}}}
	}
	b, _ := json.Marshal(root)
	return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func TestAntiObservationCoreToHTTPDatabaseColdCSV(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, stream := range []bool{false, true} {
			for _, protocol := range []string{"chat", "responses", "gemini"} {
				for _, scenario := range []string{"legal", "fallback", "auto_compaction", "unknown_finish", "code_only", "auth_replay"} {
					if (scenario == "unknown_finish" || scenario == "code_only") && channel != aistudio.ChannelBuild {
						continue
					}
					if scenario == "auto_compaction" && protocol != "responses" {
						continue
					}
					t.Run(string(channel)+"/"+protocol+map[bool]string{true: "/stream", false: "/unary"}[stream]+"/"+scenario, func(t *testing.T) {
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
						if err = aistudio.WriteStorageState(statePath, state); err != nil {
							t.Fatal(err)
						}
						accountID := "fixture-account"
						if scenario == "auth_replay" {
							accountID = "fixture@example.invalid"
							err = state.SetAuthExtension(aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", ChromeRoot: filepath.Join(dir, "unmounted-chrome"), Profile: "Default", GaiaID: "fixture-gaia", Email: accountID}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: "fixture-gaia", RefreshToken: "synthetic-token"}})
							if err != nil {
								t.Fatal(err)
							}
							if err = aistudio.WriteStorageState(statePath, state); err != nil {
								t.Fatal(err)
							}
						}
						model := aistudio.Model{ID: "fixture-model", Name: "fixture", Methods: []string{"generateContent", "countTokens"}, InputTokenLimit: 1048576, OutputTokenLimit: 32768, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true}, Channels: []string{"playground", "build"}}
						account := &aistudio.Account{ID: accountID, Directory: dir, StoragePath: statePath, RuntimePath: filepath.Join(dir, "runtime.json"), StorageState: state, Config: aistudio.DefaultAccountConfig("fixture"), State: aistudio.AccountReady, Models: []aistudio.Model{model}}
						account.Config.Enabled = true
						pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
						pool.SetUpstreamChannels([]aistudio.Channel{channel})
						if err = pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
							t.Fatal(err)
						}
						catalog := make([]any, 65)
						catalog[0] = "models/fixture-model"
						catalog[3] = "fixture"
						catalog[5] = 1048576
						catalog[6] = 32768
						catalog[7] = []any{"generateContent", "countTokens"}
						catalog[64] = []any{1, 10}
						catalogBody, _ := json.Marshal([]any{[]any{catalog}})
						plain, err := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
							return nil, errors.New("external network forbidden")
						})})
						if err != nil {
							t.Fatal(err)
						}
						preparer := &antiLedgerPreparer{state: state, fallback: scenario == "fallback", compaction: scenario == "auto_compaction", unknownFinish: scenario == "unknown_finish", otherOutput: scenario == "code_only"}
						protected, err := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{Transport: plain, Workers: aistudio.ProtectedPreparerProviderFunc(func(context.Context, string, string) (aistudio.ProtectedPreparer, error) { return preparer, nil }), SetupTimeout: time.Second})
						if err != nil {
							t.Fatal(err)
						}
						var generationTransport aistudio.ProtectedTransport = protected
						if scenario == "auth_replay" {
							preparer.authReplay = true
							refresher := &authRuntimeRefresher{pool: pool, requests: newRequestRegistry(t.Context()), refresh: func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
								return state.Cookies, nil
							}, reset: func(string) error { return nil }, prepareHeaders: func(string) (func(bool), error) { return func(bool) {}, nil }}
							generationTransport = &authRetryProtectedTransport{transport: protected, refresher: refresher}
						}
						client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: antiAuxiliaryRPC(func(_ context.Context, r aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
							if r.Method != "ListModels" {
								return nil, errors.New("unexpected auxiliary RPC")
							}
							return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(catalogBody))}, nil
						}), Protected: aistudio.ProtectedTransportFunc(func(ctx context.Context, r aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
							preparer.mu.Lock()
							preparer.name = r.Tools.Functions[len(r.Tools.Functions)-1].Name
							preparer.build = channel == aistudio.ChannelBuild
							preparer.unary = !rpc.Streaming
							preparer.mu.Unlock()
							return generationTransport.DoProtected(ctx, r, rpc)
						})})
						if err != nil {
							t.Fatal(err)
						}
						if _, err = client.ModelsForAccount(t.Context(), account.ID); err != nil {
							t.Fatal(err)
						}
						service := &antiLedgerService{Client: client, pool: pool, model: model, account: account.ID}
						path := filepath.Join(dir, "ledger.db")
						store, err := requestdb.Open(path, func(l, m string) { t.Errorf("%s %s", l, m) })
						if err != nil {
							t.Fatal(err)
						}
						defer store.Close()
						admin := &ledgerFixtureAdmin{store: store}
						handler := api.NewHandler(service, api.Config{Admin: admin})
						requested := string(channel) + "/anti-truncation/fixture-model"
						endpoint := "/v1/chat/completions"
						payload := map[string]any{"model": requested, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "prompt-private-sentinel"}}}
						if protocol == "responses" {
							endpoint = "/v1/responses"
							payload = map[string]any{"model": requested, "stream": stream, "input": "prompt-private-sentinel"}
						}
						if scenario == "auto_compaction" {
							payload["context_management"] = []any{map[string]any{"type": "compaction", "compact_threshold": 1}}
							payload["input"] = []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 9000)}, map[string]any{"role": "assistant", "content": strings.Repeat("past answer ", 9000)}, map[string]any{"role": "user", "content": "current"}}
						}
						if protocol == "gemini" {
							action := "generateContent"
							if stream {
								action = "streamGenerateContent"
							}
							endpoint = "/v1beta/models/" + requested + ":" + action
							payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "prompt-private-sentinel"}}}}}
						}
						encoded, _ := json.Marshal(payload)
						req := httptest.NewRequest("POST", endpoint, bytes.NewReader(encoded))
						req.Header.Set("Content-Type", "application/json")
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						if rec.Code != 200 {
							t.Fatalf("HTTP=%d %s", rec.Code, rec.Body.String())
						}
						wantLives := 1
						if scenario == "auto_compaction" {
							wantLives = 2
						}
						if len(admin.finals) != 1 || len(admin.finals[0].AntiObservations) != wantLives {
							t.Fatalf("observer missing: %+v", admin.finals)
						}
						value := admin.finals[0].AntiObservations[wantLives-1]
						wantCalls := 1
						if scenario != "legal" {
							wantCalls = 0
						}
						if scenario == "unknown_finish" {
							wantCalls = 1
						}
						wantRounds := 1
						if scenario == "auth_replay" {
							wantRounds = 2
							wantCalls = 1
						}
						round := value.Rounds[len(value.Rounds)-1]
						wantComplete := scenario != "auth_replay"
						if value.Complete != wantComplete || len(value.Rounds) != wantRounds || round.Calls != wantCalls || round.Legal != wantCalls || value.Purpose != "reply" || value.ParentID != admin.finals[0].RequestID {
							t.Fatalf("core evidence: %+v", value)
						}
						if (scenario == "fallback" || scenario == "auto_compaction") && !value.Rounds[0].Fallback {
							t.Fatal("actual fallback missing")
						}
						if scenario == "code_only" && (!value.Rounds[0].OtherOutput || value.Rounds[0].Fallback) {
							t.Fatal("code-only misclassified")
						}
						if scenario == "unknown_finish" && value.Rounds[0].FinishReason != "provider_unknown" {
							t.Fatal("uncontrolled enum persisted")
						}
						if scenario == "auto_compaction" {
							summary := admin.finals[0].AntiObservations[0]
							if summary.ID == value.ID || summary.RequestID != value.RequestID || summary.Purpose != "compaction" || summary.Rounds[0].Calls != 1 || summary.Rounds[0].Legal != 1 {
								t.Fatalf("summary replaced reply: %+v", admin.finals[0].AntiObservations)
							}
						}
						if scenario == "auth_replay" && (value.Rounds[0].Status != 401 || value.Rounds[0].Calls != 0 || value.Rounds[0].AttemptID == round.AttemptID) {
							t.Fatalf("replay counts: %+v", value)
						}
						if admin.finals[0].ToolCalls != 0 || *admin.finals[0].SentAttemptCount != wantLives+wantRounds-1 {
							t.Fatal("internal tool changed physical/public accounting")
						}
						if len(admin.finalLogs[0].AntiObservations) != wantLives {
							t.Fatal("structured log lost evidence")
						}
						if err = store.Close(); err != nil {
							t.Fatal(err)
						}
						store, err = requestdb.Open(path, func(l, m string) { t.Errorf("%s %s", l, m) })
						if err != nil {
							t.Fatal(err)
						}
						defer store.Close()
						query := api.UsageRecordQuery{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Limit: 10}
						page, err := store.Records(t.Context(), query)
						if err != nil || len(page.Items) != 1 || len(page.Items[0].AntiObservations) != wantLives {
							t.Fatalf("cold=%+v %v", page, err)
						}
						cold := page.Items[0].AntiObservations[wantLives-1]
						if cold.Rounds[len(cold.Rounds)-1].Calls != wantCalls || cold.Rounds[len(cold.Rounds)-1].FinishReason != round.FinishReason {
							t.Fatalf("cold drift: %+v", cold)
						}
						var csvOut bytes.Buffer
						if err = store.ExportRecords(t.Context(), query, &csvOut); err != nil {
							t.Fatal(err)
						}
						cells, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(csvOut.String(), "\xef\xbb\xbf"))).ReadAll()
						if err != nil {
							t.Fatal(err)
						}
						var exported []aistudio.AntiTruncationObservation
						if err = json.Unmarshal([]byte(cells[1][28]), &exported); err != nil || len(exported) != wantLives || exported[wantLives-1].Rounds[len(exported[wantLives-1].Rounds)-1].Calls != wantCalls {
							t.Fatal("CSV lost evidence", err)
						}
						newJSON, _ := json.Marshal(cold)
						if strings.Contains(string(newJSON), "private-sentinel") || strings.Contains(string(newJSON), "visible-fixture") || strings.Contains(string(newJSON), "aistudio2api_emit_v1") || strings.Contains(strings.ToLower(string(newJSON)), "synthetic_private_marker") {
							t.Fatal("new observation exposed payload")
						}
						if _, err := store.RequestBody(t.Context(), page.Items[0].ID); !errors.Is(err, api.ErrRequestBodyNotFound) {
							t.Fatal("body capture unexpectedly enabled", err)
						}
						if *antiEvidenceDirectory != "" {
							if err := store.Close(); err != nil {
								t.Fatal(err)
							}
							if err := os.MkdirAll(*antiEvidenceDirectory, 0700); err != nil {
								t.Fatal(err)
							}
							name := strings.NewReplacer("/", "-", "\\", "-").Replace(t.Name())
							data, err := os.ReadFile(path)
							if err != nil {
								t.Fatal(err)
							}
							if err = os.WriteFile(filepath.Join(*antiEvidenceDirectory, name+".db"), data, 0600); err != nil {
								t.Fatal(err)
							}
							if err = os.WriteFile(filepath.Join(*antiEvidenceDirectory, name+".csv"), csvOut.Bytes(), 0600); err != nil {
								t.Fatal(err)
							}
						}
					})
				}
			}
		}
	}
}
