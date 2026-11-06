package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

type quotaHTTPView struct {
	store   *requestdb.Store
	account string
}

func (v *quotaHTTPView) Quota(ctx context.Context) (api.QuotaReport, error) {
	return v.store.QuotaSnapshot(ctx, []api.QuotaMember{{ID: v.account, Label: "Fixture", Tier: "Ultra", Enabled: true, State: "ready"}}, time.Now())
}
func quotaHTTPRead(t *testing.T, handler http.Handler) api.QuotaReport {
	t.Helper()
	req := httptest.NewRequest("GET", "http://localhost/api/usage/quota", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var report api.QuotaReport
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &report) != nil {
		t.Fatalf("quota HTTP: %d %s", rec.Code, rec.Body.String())
	}
	for _, private := range []string{"prompt-private-sentinel", "visible-fixture", "synthetic-not-a-credential", "cookies", "storage_path", "proxy"} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatalf("quota leaked %s", private)
		}
	}
	return report
}
func completeQuotaFixture(t *testing.T, path string) *requestdb.Store {
	t.Helper()
	store, err := requestdb.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	// Only fixture coverage metadata is seeded. All consumption below comes from native decoding.
	_, err = db.Exec(`UPDATE quota17_tracking SET observed_since=? WHERE id=1`, requestdb.QuotaWindow(time.Now()).Add(-24*time.Hour).UnixMilli())
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err = requestdb.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func quotaHTTPGenerate(t *testing.T, handler http.Handler, model, protocol string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	endpoint := "/v1/chat/completions"
	payload := map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "prompt-private-sentinel"}}}
	if protocol == "responses" {
		endpoint = "/v1/responses"
		payload = map[string]any{"model": model, "stream": stream, "input": "prompt-private-sentinel"}
	}
	if protocol == "gemini" {
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		endpoint = "/v1beta/models/" + model + ":" + action
		payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "prompt-private-sentinel"}}}}}
	}
	encoded, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", endpoint, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestQuotaClientHTTPColdReadAndReadOnlyDisplay(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, anti := range []bool{false, true} {
				name := protocol + map[bool]string{true: "/sse", false: "/json"}[stream] + map[bool]string{true: "/anti", false: "/ordinary"}[anti]
				t.Run(name, func(t *testing.T) {
					scenario := "measured"
					if anti {
						scenario = "atomic_anti"
					}
					service, preparer := newOutputHTTPFixture(t, aistudio.ChannelPlayground, scenario, "gemini-4-argon")
					path := filepath.Join(t.TempDir(), "quota.db")
					store := completeQuotaFixture(t, path)
					defer store.Close()
					service.quotaObserver = store.RecordQuotaAttempt
					view := &quotaHTTPView{store: store, account: service.account}
					admin := &ledgerFixtureAdmin{store: store}
					handler := api.NewHandler(service, api.Config{Admin: admin, Ledger: store, Quota: view})
					requested := "playground/gemini-4-argon"
					if anti {
						requested = "playground/anti-truncation/gemini-4-argon"
					}
					rec := quotaHTTPGenerate(t, handler, requested, protocol, stream)
					if rec.Code != 200 || !strings.Contains(rec.Body.String(), "visible") {
						t.Fatalf("generation %d %s", rec.Code, rec.Body.String())
					}
					report := quotaHTTPRead(t, handler)
					if report.Tokens.Used == nil || *report.Tokens.Used != 1520 || report.Requests.Used == nil || *report.Requests.Used != 1 || report.Tokens.Remaining == nil || *report.Tokens.Remaining != 30000000-1520 {
						t.Fatalf("real producer counts: %+v", report)
					}
					if preparer.calls != 1 {
						t.Fatalf("observer changed sends: %d", preparer.calls)
					}
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := requestdb.Open(path, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					view.store = reopened
					cold := quotaHTTPRead(t, api.NewHandler(service, api.Config{Ledger: reopened, Quota: view}))
					if cold.Tokens.Used == nil || *cold.Tokens.Used != 1520 || cold.Requests.Used == nil || *cold.Requests.Used != 1 {
						t.Fatalf("cold quota: %+v", cold)
					}
				})
			}
		}
	}
}

func TestQuotaNativeMissingUsageAnd429(t *testing.T) {
	for _, scenario := range []string{"no_usage", "http429"} {
		t.Run(scenario, func(t *testing.T) {
			service, preparer := newOutputHTTPFixture(t, aistudio.ChannelPlayground, scenario, "gemini-4-argon")
			store := completeQuotaFixture(t, filepath.Join(t.TempDir(), "quota.db"))
			defer store.Close()
			service.quotaObserver = store.RecordQuotaAttempt
			view := &quotaHTTPView{store: store, account: service.account}
			handler := api.NewHandler(service, api.Config{Admin: &ledgerFixtureAdmin{store: store}, Quota: view})
			rec := quotaHTTPGenerate(t, handler, "playground/gemini-4-argon", "chat", false)
			want := 200
			if scenario == "http429" {
				want = 429
			}
			if rec.Code != want {
				t.Fatalf("status %d want %d: %s", rec.Code, want, rec.Body.String())
			}
			report := quotaHTTPRead(t, handler)
			if report.Requests.Used == nil || *report.Requests.Used != 1 || preparer.calls != 1 {
				t.Fatalf("wrong physical counts: %+v", report)
			}
			if report.Tokens.Used == nil || *report.Tokens.Used != 0 || report.Unreported != 1 {
				t.Fatalf("invented tokens without upstream usage: %+v", report)
			}
			if scenario == "http429" && (report.Tokens.Remaining == nil || *report.Tokens.Remaining != 0 || report.Requests.Remaining == nil || *report.Requests.Remaining != 0) {
				t.Fatal("429 didn't clear both display balances")
			}
			if scenario == "http429" {
				// Keep the same pool and zero display; only the fake response becomes healthy.
				preparer.scenario = "measured"
				again := quotaHTTPGenerate(t, handler, "playground/gemini-4-argon", "chat", false)
				if again.Code != 200 || preparer.calls != 2 {
					t.Fatal("zero display blocked generation")
				}
				if quotaHTTPRead(t, handler).Accounts[0].Exhausted429 != true {
					t.Fatal("successful generation reset the 429 display latch")
				}
			}
		})
	}
}

func TestQuotaAdminAuthenticationAndNoWriteRoute(t *testing.T) {
	service, _ := newOutputHTTPFixture(t, aistudio.ChannelPlayground, "measured", "gemini-4-argon")
	store := completeQuotaFixture(t, filepath.Join(t.TempDir(), "quota.db"))
	defer store.Close()
	view := &quotaHTTPView{store: store, account: service.account}
	handler := api.NewHandler(service, api.Config{Quota: view, AdminAuthEnabled: true, AdminUsername: "fixture", AdminPassword: "fixture-not-real"})
	req := httptest.NewRequest("GET", "http://localhost/api/usage/quota", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated quota status=%d", rec.Code)
	}
	handler = api.NewHandler(service, api.Config{Quota: view})
	req = httptest.NewRequest("POST", "http://localhost/api/usage/quota", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("quota unexpectedly accepts writes")
	}
}
