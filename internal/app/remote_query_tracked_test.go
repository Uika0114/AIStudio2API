package app

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type queryTrackedBackend struct {
	aistudio.Service
	f            *queryAppFixture
	calls, sends int
	mode         string
}

func (b *queryTrackedBackend) CachedModels() []aistudio.Model { return b.f.account.Models }
func (b *queryTrackedBackend) RefreshAccountModels(context.Context, string) ([]aistudio.Model, error) {
	return nil, errors.New("unexpected model refresh")
}
func (b *queryTrackedBackend) RefreshAccountState(context.Context, string, bool) (aistudio.AccountRefresh, error) {
	return aistudio.AccountRefresh{}, errors.New("unexpected account refresh")
}
func (b *queryTrackedBackend) Generate(ctx context.Context, r aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	b.calls++
	if b.mode == "delivered" || b.mode == "eof" {
		ch := make(chan aistudio.Event, 2)
		if b.mode == "delivered" {
			ch <- aistudio.Event{Kind: aistudio.EventText, Text: "invented visible text"}
		}
		ch <- aistudio.Event{Kind: aistudio.EventError, Err: errors.Join(io.ErrUnexpectedEOF, aistudio.SessionError("INDETERMINATE"))}
		close(ch)
		return ch, nil
	}
	if b.mode == "worker-init" {
		return nil, &accountWorkerInitError{err: b.f.refresher.Recover(ctx, &aistudio.RPCError{StatusCode: 401, Code: 16}), authHandled: true}
	}
	_, err := b.f.refresher.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
		b.sends++
		return nil, &aistudio.RPCError{StatusCode: 401, Code: 16}
	})
	return nil, err
}

func TestRemoteQueryTrackedGenerateNeverFailsOverOrRestartsOutput(t *testing.T) {
	for _, mode := range []string{"rpc", "worker-init", "delivered", "eof"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueryAppFixture(t, "not-started", "reauth", true)
			lease, _ := aistudio.AccountLeaseFromContext(f.ctx)
			if e := lease.Release(); e != nil {
				t.Fatal(e)
			}
			// Make failover genuinely available, rather than proving no retry with one account.
			second := &aistudio.Account{ID: "other-query@example.invalid", Config: aistudio.DefaultAccountConfig("other-query@example.invalid"), Directory: t.TempDir(), State: aistudio.AccountReady, Models: append([]aistudio.Model(nil), f.account.Models...)}
			second.Config.Enabled = true
			second.StoragePath = filepath.Join(second.Directory, "storage-state.json")
			second.RuntimePath = filepath.Join(second.Directory, "runtime.json")
			if e := aistudio.WriteStorageState(second.StoragePath, aistudio.StorageState{Cookies: fixtureRefreshCookies("second-private-sentinel")}); e != nil {
				t.Fatal(e)
			}
			second.StorageState, _ = aistudio.LoadStorageState(second.StoragePath)
			if e := f.pool.Add(second); e != nil {
				t.Fatal(e)
			}
			backend := &queryTrackedBackend{f: f, mode: mode}
			registry := f.refresher.requests
			workers := &accountWorkerManager{pool: f.pool, accounts: map[string]*accountWorker{}, lifecycle: t.Context(), requests: registry}
			tracked := newTrackedService(t.Context(), backend, f.pool, registry, workers, time.Second)
			tracked.dataContext = t.Context()
			tracked.state.Store(serviceRunning)
			tracked.proxies = aistudio.NewProxyPool(f.pool, nil)
			tracked.leaseAcquire = func(c context.Context, s aistudio.AccountSelection) (*aistudio.AccountLease, error) {
				s.AccountID = f.account.ID
				l, _, e := f.pool.TryAcquireFor(c, s)
				return l, e
			}
			request := aistudio.GenerateRequest{ID: "query-tracked-" + mode, Model: "playground/query-fixture-model", Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "invented prompt"}}}}}
			events, e := tracked.Generate(t.Context(), request)
			if e != nil {
				t.Fatal(e)
			}
			var terminal error
			text := 0
			for event := range events {
				if event.Kind == aistudio.EventError {
					terminal = event.Err
				}
				if event.Kind == aistudio.EventText {
					text++
				}
			}
			want := "NOT_STARTED"
			if mode == "eof" || mode == "delivered" {
				want = "INDETERMINATE"
			}
			if !aistudio.IsSessionFailureCode(terminal, want) || backend.calls != 1 || backend.sends > 1 || f.auth.Load() != 0 {
				t.Fatal("outer retry/failover or missing failure", terminal, backend.calls, backend.sends)
			}
			if mode == "delivered" && text != 1 {
				t.Fatal("delivered output lost or duplicated")
			}
			registry.mu.Lock()
			defer registry.mu.Unlock()
			queryEvents := 0
			for _, entry := range registry.logs {
				if strings.Contains(entry.Message, "账号切换") || strings.Contains(entry.Message, "Worker 重建") || strings.Contains(entry.Message, "private-sentinel") {
					t.Fatal("query failure escaped safety boundary")
				}
				if strings.HasPrefix(entry.Message, "remote_auth ") {
					queryEvents++
					if entry.Request == nil || entry.Request.ID != request.ID || entry.Event != "request.progress" {
						t.Fatal("remote diagnosis lacks tracked execution attribution")
					}
				}
			}
			if (mode == "rpc" || mode == "worker-init") && queryEvents == 0 {
				t.Fatal("missing query diagnostics")
			}
		})
	}
}
