package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// This fixture explicitly models a Go source-read lineage. It does not make an
// external request, start a browser or load an installed account/profile.
type outputHTTPBody struct {
	chunks        [][]byte
	index, offset int
	trusted       bool
	onClose       context.CancelFunc
}

func (body *outputHTTPBody) OutputReadSourceKnown() bool { return body.trusted }
func (body *outputHTTPBody) Close() error {
	if body.onClose != nil {
		body.onClose()
	}
	return nil
}
func (body *outputHTTPBody) Read(destination []byte) (int, error) {
	if body.index == len(body.chunks) {
		return 0, io.EOF
	}
	// Distinct synthetic deliveries must not collapse into the same platform
	// clock tick. This is a fixture delay, not a product minimum-window rule.
	if body.index > 0 && body.offset == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	count := copy(destination, body.chunks[body.index][body.offset:])
	body.offset += count
	if body.offset == len(body.chunks[body.index]) {
		body.index++
		body.offset = 0
	}
	return count, nil
}

type outputHTTPPreparer struct {
	state             aistudio.StorageState
	mu                sync.Mutex
	calls             int
	build, unary      bool
	name, scenario    string
	cancelAfterDecode context.CancelFunc
}

func (p *outputHTTPPreparer) Prepare(_ context.Context, r aistudio.ProtectedRequest) (aistudio.PreparedProtectedRequest, error) {
	return aistudio.PreparedProtectedRequest{Headers: r.Headers.Clone(), Body: r.Body}, nil
}
func (p *outputHTTPPreparer) BrowserStorageState(context.Context) (aistudio.StorageState, error) {
	return p.state, nil
}
func (p *outputHTTPPreparer) SendProtected(context.Context, aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	frame := func(text string, count int64, withCount, finish, atomic bool) []byte {
		if p.build {
			part := map[string]any{"text": text}
			if atomic {
				part = map[string]any{"functionCall": map[string]any{"name": p.name, "args": map[string]any{"content": text}}}
			}
			candidate := map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}}
			if finish {
				candidate["finishReason"] = "STOP"
			}
			response := map[string]any{"candidates": []any{candidate}}
			if withCount {
				response["usageMetadata"] = map[string]any{"promptTokenCount": 1000, "candidatesTokenCount": count, "thoughtsTokenCount": 77, "totalTokenCount": 1077 + count}
			}
			inner, _ := json.Marshal(response)
			outer, _ := json.Marshal([]any{string(inner)})
			return outer
		}
		part := []any{nil, text}
		if atomic {
			part = make([]any, 11)
			part[10] = []any{p.name, []any{[]any{[]any{"content", []any{nil, nil, text}}}}}
		}
		var end any
		if finish {
			end = 1
		}
		candidate := []any{[]any{[]any{part}, "model"}, end}
		var usage any
		if withCount {
			usage = []any{1000, count, 1077 + count, 0, nil, nil, nil, nil, nil, 77}
		}
		encoded, _ := json.Marshal([]any{[]any{candidate}, nil, usage})
		return encoded
	}
	var chunks [][]byte
	if p.build && p.unary {
		chunks = [][]byte{frame("visible-fixture", 620, p.scenario != "missing_count", true, p.scenario == "atomic_anti")}
		if p.scenario == "broken" {
			chunks[0] = chunks[0][:len(chunks[0])-1]
		}
	} else if p.scenario == "atomic_anti" {
		if p.name == "" {
			return nil, errors.New("fixture expected an injected transport tool")
		}
		chunks = [][]byte{append(append([]byte("[["), frame("visible-fixture", 620, true, true, true)...), []byte("]]")...)}
	} else {
		first, last := frame("visible-", 100, true, false, false), frame("fixture", 620, p.scenario != "missing_count", true, false)
		chunks = [][]byte{append(append([]byte("[["), first...), ','), append(last, []byte("]]")...)}
		if p.scenario == "single" {
			chunks = [][]byte{append(chunks[0], chunks[1]...)}
		}
		if p.scenario == "broken" {
			chunks[1] = chunks[1][:len(chunks[1])-2]
		}
	}
	return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: &outputHTTPBody{chunks: chunks, trusted: p.scenario != "untrusted", onClose: p.cancelAfterDecode}}, nil
}

func newOutputHTTPFixture(t *testing.T, channel aistudio.Channel, scenario string) (*antiLedgerService, *outputHTTPPreparer) {
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
	if err = aistudio.WriteStorageState(statePath, state); err != nil {
		t.Fatal(err)
	}
	model := aistudio.Model{ID: "fixture-model", Name: "fixture", Methods: []string{"generateContent", "countTokens"}, InputTokenLimit: 1048576, OutputTokenLimit: 32768, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true}, Channels: []string{"playground", "build"}}
	account := &aistudio.Account{ID: "fixture-account", Directory: dir, StoragePath: statePath, RuntimePath: filepath.Join(dir, "runtime.json"), StorageState: state, Config: aistudio.DefaultAccountConfig("fixture"), State: aistudio.AccountReady, Models: []aistudio.Model{model}}
	account.Config.Enabled = true
	pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
	pool.SetUpstreamChannels([]aistudio.Channel{channel})
	if err = pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
		t.Fatal(err)
	}
	catalog := make([]any, 65)
	catalog[0], catalog[3], catalog[5], catalog[6], catalog[7], catalog[64] = "models/fixture-model", "fixture", 1048576, 32768, []any{"generateContent", "countTokens"}, []any{1, 10}
	catalogBody, _ := json.Marshal([]any{[]any{catalog}})
	plain, err := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
		return nil, errors.New("fixture external network forbidden")
	})})
	if err != nil {
		t.Fatal(err)
	}
	preparer := &outputHTTPPreparer{state: state, scenario: scenario, build: channel == aistudio.ChannelBuild}
	protected, err := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{Transport: plain, Workers: aistudio.ProtectedPreparerProviderFunc(func(context.Context, string, string) (aistudio.ProtectedPreparer, error) { return preparer, nil }), SetupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: antiAuxiliaryRPC(func(_ context.Context, r aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		if r.Method != "ListModels" {
			return nil, errors.New("fixture unexpected auxiliary RPC")
		}
		return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(catalogBody))}, nil
	}), Protected: aistudio.ProtectedTransportFunc(func(ctx context.Context, r aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		preparer.mu.Lock()
		preparer.unary = !rpc.Streaming
		if len(r.Tools.Functions) > 0 {
			preparer.name = r.Tools.Functions[len(r.Tools.Functions)-1].Name
		}
		preparer.mu.Unlock()
		return protected.DoProtected(ctx, r, rpc)
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.ModelsForAccount(t.Context(), account.ID); err != nil {
		t.Fatal(err)
	}
	return &antiLedgerService{Client: client, pool: pool, model: model, account: account.ID}, preparer
}

func TestOutputTimingClientHTTPColdLedger(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, stream := range []bool{false, true} {
			for _, protocol := range []string{"chat", "responses", "gemini"} {
				for _, scenario := range []string{"measured", "single", "untrusted", "missing_count", "atomic_anti", "fallback_anti", "broken", "auto_compaction", "cancel_after_measurement"} {
					if scenario == "auto_compaction" && protocol != "responses" {
						continue
					}
					if scenario == "cancel_after_measurement" && (!stream || protocol != "responses") {
						continue
					}
					t.Run(string(channel)+"/"+protocol+map[bool]string{true: "/stream", false: "/unary"}[stream]+"/"+scenario, func(t *testing.T) {
						service, preparer := newOutputHTTPFixture(t, channel, scenario)
						path := filepath.Join(t.TempDir(), "ledger.db")
						logger := func(level, message string) { t.Errorf("%s %s", level, message) }
						store, err := requestdb.Open(path, logger)
						if err != nil {
							t.Fatal(err)
						}
						defer store.Close()
						admin := &ledgerFixtureAdmin{store: store}
						handler := api.NewHandler(service, api.Config{Admin: admin})
						requested := string(channel) + "/fixture-model"
						if strings.HasSuffix(scenario, "_anti") {
							requested = string(channel) + "/anti-truncation/fixture-model"
						}
						endpoint := "/v1/chat/completions"
						payload := map[string]any{"model": requested, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "prompt-private-sentinel"}}}
						if protocol == "responses" {
							endpoint = "/v1/responses"
							payload = map[string]any{"model": requested, "stream": stream, "input": "prompt-private-sentinel"}
						}
						if scenario == "auto_compaction" {
							payload["context_management"] = []any{map[string]any{"type": "compaction", "compact_threshold": 1}}
							payload["input"] = []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 9000)}, map[string]any{"role": "assistant", "content": strings.Repeat("old answer ", 9000)}, map[string]any{"role": "user", "content": "current"}}
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
						if scenario == "cancel_after_measurement" {
							ctx, cancel := context.WithCancel(req.Context())
							defer cancel()
							req = req.WithContext(ctx)
							preparer.cancelAfterDecode = cancel
						}
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						if scenario != "broken" && scenario != "cancel_after_measurement" && (rec.Code != 200 || !strings.Contains(rec.Body.String(), "visible")) {
							t.Fatalf("response %d %s", rec.Code, rec.Body.String())
						}
						if len(admin.finals) != 1 {
							t.Fatalf("missing root record: %+v", admin.finals)
						}
						wantCalls := 1
						if scenario == "auto_compaction" {
							wantCalls = 2
						}
						if preparer.calls != wantCalls {
							t.Fatalf("observation changed physical send count: %d", preparer.calls)
						}
						if scenario == "cancel_after_measurement" {
							attempts := admin.finals[0].Attempts
							if req.Context().Err() != context.Canceled || len(attempts) != 1 || attempts[0].OutputTiming == nil || attempts[0].OutputTiming.TPS() == nil {
								t.Fatalf("fixture must cancel only after the physical metric was measured: %+v", admin.finals[0])
							}
						}
						if scenario == "auto_compaction" {
							attempts := admin.finals[0].Attempts
							if len(attempts) != 2 || attempts[0].OutputTiming == nil || attempts[1].OutputTiming == nil || attempts[0].OutputTiming.Purpose != "compaction" || attempts[1].OutputTiming.Purpose != "reply" || attempts[0].OutputTiming.LifecycleID == attempts[1].OutputTiming.LifecycleID {
								t.Fatalf("summary/reply execution identity mixed: %+v", attempts)
							}
						}
						if err = store.Close(); err != nil {
							t.Fatal(err)
						}
						store, err = requestdb.Open(path, logger)
						if err != nil {
							t.Fatal(err)
						}
						defer store.Close()
						page, err := store.Records(t.Context(), api.UsageRecordQuery{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Limit: 10})
						if err != nil || len(page.Items) != 1 {
							t.Fatalf("cold row: %+v %v", page, err)
						}
						row := page.Items[0]
						if scenario == "cancel_after_measurement" && row.State != "cancelled" {
							t.Fatalf("late cancelled root misclassified: %+v", row)
						}
						want := (scenario == "measured" || scenario == "fallback_anti" || scenario == "auto_compaction") && (channel != aistudio.ChannelBuild || stream)
						if want {
							if row.OutputTPS == nil || row.OutputTiming == nil || row.OutputTiming.StartOutputTokens != 100 || row.OutputTiming.EndOutputTokens != 620 || row.OutputTiming.DurationMS <= 0 || *row.OutputTPS != 520000/row.OutputTiming.DurationMS {
								t.Fatalf("real chain missing/wrong metric: %+v timing=%+v", row, row.OutputTiming)
							}
						} else if row.OutputTPS != nil {
							t.Fatalf("unsupported/incomplete chain exposed TPS: %+v", row)
						}
						if row.Model != "fixture-model" || row.RequestedModel != requested || row.ToolCalls != 0 {
							t.Fatalf("metric changed model/tool accounting: %+v", row)
						}
						body, _ := json.Marshal(row)
						if bytes.Contains(body, []byte("prompt-private-sentinel")) || bytes.Contains(body, []byte("visible-fixture")) || bytes.Contains(body, []byte("synthetic-not-a-credential")) || bytes.Contains(body, []byte(`"output_timing"`)) {
							t.Fatalf("record leaked payload/evidence internals: %s", body)
						}
					})
				}
			}
		}
	}
}
