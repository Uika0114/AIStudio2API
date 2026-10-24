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

// Deliver one complete raw frame, then stop below the decoder. Close unblocks
// the source without introducing a synthetic successful terminal event.
type antiInterruptedBody struct {
	prefix          *bytes.Reader
	entered, closed chan struct{}
	enter, close    sync.Once
}

func (b *antiInterruptedBody) Read(dst []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(dst)
	}
	b.enter.Do(func() { close(b.entered) })
	<-b.closed
	return 0, context.Canceled
}
func (b *antiInterruptedBody) Close() error { b.close.Do(func() { close(b.closed) }); return nil }

type antiCancelledPreparer struct {
	antiLedgerPreparer
	body    *antiInterruptedBody
	forward bool
}

func (p *antiCancelledPreparer) SendProtected(ctx context.Context, r aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	response, err := p.antiLedgerPreparer.SendProtected(ctx, r)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	// Strip finish from a complete fixture, preserving the exact PG/Build envelope.
	if p.build {
		var outer []any
		json.Unmarshal(b, &outer)
		chunk := outer[0].([]any)[0].([]any)[0].(string)
		var value map[string]any
		json.Unmarshal([]byte(chunk), &value)
		delete(value["candidates"].([]any)[0].(map[string]any), "finishReason")
		if p.forward && p.fallback {
			value["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["thought"] = true
		}
		raw, _ := json.Marshal(value)
		b, _ = json.Marshal([]any{[]any{[]any{string(raw)}}})
	} else {
		var outer []any
		json.Unmarshal(b, &outer)
		candidate := outer[0].([]any)[0].([]any)[0].([]any)[0].([]any)
		candidate[1] = nil
		if p.forward && p.fallback {
			part := candidate[0].([]any)[0].([]any)[0].([]any)
			for len(part) <= 12 {
				part = append(part, nil)
			}
			part[12] = true
			candidate[0].([]any)[0].([]any)[0] = part
		}
		b, _ = json.Marshal(outer)
	}
	p.body.prefix = bytes.NewReader(append(b[:len(b)-2], ','))
	response.Body = p.body
	return response, nil
}

type antiWriteBarrier struct {
	*httptest.ResponseRecorder
	written chan struct{}
	once    sync.Once
}

func (w *antiWriteBarrier) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if bytes.Contains(p, []byte("visible-fixture")) {
		w.once.Do(func() { close(w.written) })
	}
	return n, err
}

func TestAntiObservationCancelledCoreTrackedColdEvidence(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		for _, hit := range []bool{false, true} {
			for _, forward := range []bool{false, true} {
				phase := "/decoder_boundary"
				if forward {
					phase = "/after_http_write"
				}
				t.Run(string(channel)+map[bool]string{false: "/unknown", true: "/positive"}[hit]+phase, func(t *testing.T) {
					dir := t.TempDir()
					statePath := filepath.Join(dir, "synthetic-state.json")
					state := aistudio.StorageState{Cookies: fixtureRefreshCookies("synthetic-cookie")}
					if err := aistudio.WriteStorageState(statePath, state); err != nil {
						t.Fatal(err)
					}
					model := aistudio.Model{ID: "fixture-model", Methods: []string{"generateContent"}, OutputTokenLimit: 32768, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true}}
					account := &aistudio.Account{ID: "fixture", Directory: dir, StoragePath: statePath, RuntimePath: filepath.Join(dir, "runtime.json"), StorageState: state, Config: aistudio.DefaultAccountConfig("fixture"), State: aistudio.AccountReady, Models: []aistudio.Model{model}}
					account.Config.Enabled = true
					pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
					pool.SetUpstreamChannels([]aistudio.Channel{channel})
					if err := pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
						t.Fatal(err)
					}
					catalog := make([]any, 65)
					catalog[0] = "models/fixture-model"
					catalog[3] = "fixture"
					catalog[6] = 32768
					catalog[7] = []any{"generateContent"}
					catalog[64] = []any{1, 10}
					catalogBody, _ := json.Marshal([]any{[]any{catalog}})
					plain, err := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
						return nil, errors.New("external network forbidden")
					})})
					if err != nil {
						t.Fatal(err)
					}
					interrupted := &antiInterruptedBody{entered: make(chan struct{}), closed: make(chan struct{})}
					defer interrupted.Close()
					preparer := &antiCancelledPreparer{antiLedgerPreparer: antiLedgerPreparer{state: state, build: channel == aistudio.ChannelBuild, fallback: !hit}, body: interrupted, forward: forward}
					worker, err := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{Transport: plain, Workers: aistudio.ProtectedPreparerProviderFunc(func(context.Context, string, string) (aistudio.ProtectedPreparer, error) { return preparer, nil }), SetupTimeout: time.Second})
					if err != nil {
						t.Fatal(err)
					}
					client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: antiAuxiliaryRPC(func(_ context.Context, r aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
						if r.Method != "ListModels" {
							return nil, errors.New("unexpected RPC")
						}
						return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(catalogBody))}, nil
					}), Protected: aistudio.ProtectedTransportFunc(func(ctx context.Context, r aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
						preparer.name = r.Tools.Functions[len(r.Tools.Functions)-1].Name
						return worker.DoProtected(ctx, r, rpc)
					})})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = client.ModelsForAccount(t.Context(), account.ID); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(dir, "ledger.db")
					store, err := requestdb.Open(path, func(l, m string) { t.Errorf("%s %s", l, m) })
					if err != nil {
						t.Fatal(err)
					}
					defer store.Close()
					admin := &ledgerFixtureAdmin{store: store}
					handler := api.NewHandler(&antiLedgerService{Client: client, pool: pool, account: account.ID, model: model}, api.Config{Admin: admin})
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					modelName := string(channel) + "/anti-truncation/fixture-model"
					req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+modelName+`","stream":true,"messages":[{"role":"user","content":"private-prompt"}]}`)).WithContext(ctx)
					req.Header.Set("Content-Type", "application/json")
					done := make(chan struct{})
					recorder := &antiWriteBarrier{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{})}
					go func() { defer close(done); handler.ServeHTTP(recorder, req) }()
					select {
					case <-interrupted.entered:
					case <-time.After(5 * time.Second):
						t.Fatal("decoder did not reach blocking boundary")
					}
					if forward {
						select {
						case <-recorder.written:
						case <-time.After(5 * time.Second):
							t.Fatal("first forwarded content never reached HTTP writer")
						}
					}
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("root waited for generation cleanup")
					}
					if len(admin.finals) != 1 || len(admin.finals[0].AntiObservations) != 1 {
						t.Fatal("cancel evidence absent")
					}
					v := admin.finals[0].AntiObservations[0]
					want := 0
					if hit {
						want = 1
					}
					if v.Complete || len(v.Rounds) != 1 || v.Rounds[0].Calls != want || !v.Sent {
						t.Fatalf("cancel coverage fabricated: %+v", v)
					}
					if err = store.Close(); err != nil {
						t.Fatal(err)
					}
					store, err = requestdb.Open(path, func(l, m string) { t.Errorf("%s %s", l, m) })
					if err != nil {
						t.Fatal(err)
					}
					defer store.Close()
					page, err := store.Records(t.Context(), api.UsageRecordQuery{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Limit: 10})
					if err != nil || len(page.Items) != 1 {
						t.Fatal("cold cancel missing", err)
					}
					cold := page.Items[0].AntiObservations[0]
					if cold.Complete || cold.Rounds[0].Calls != want {
						t.Fatal("cancel cold drift")
					}
					if *antiEvidenceDirectory != "" {
						if err = store.Close(); err != nil {
							t.Fatal(err)
						}
						if err = os.MkdirAll(*antiEvidenceDirectory, 0700); err != nil {
							t.Fatal(err)
						}
						b, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						name := strings.ReplaceAll(t.Name(), "/", "-")
						if err = os.WriteFile(filepath.Join(*antiEvidenceDirectory, name+".db"), b, 0600); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}
