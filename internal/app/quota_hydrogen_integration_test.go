package app

import (
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

func completeHydrogenQuotaFixture(t *testing.T, path string) *requestdb.Store {
	t.Helper()
	store := completeQuotaFixture(t, path)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE quota_hydrogen_tracking SET observed_since=? WHERE id=1`, requestdb.QuotaWindow(time.Now()).Add(-24*time.Hour).UnixMilli())
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

func hydrogenQuotaHTTPRead(t *testing.T, handler http.Handler) api.QuotaReport {
	t.Helper()
	req := httptest.NewRequest("GET", "http://localhost/api/usage/quota?model=gemini-4-hydrogen", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var report api.QuotaReport
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &report) != nil || report.Model != "gemini-4-hydrogen" {
		t.Fatalf("Hydrogen quota HTTP: %d %s", rec.Code, rec.Body.String())
	}
	for _, private := range []string{"prompt-private-sentinel", "visible-fixture", "synthetic-not-a-credential", "cookies", "storage_path", "proxy"} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatalf("Hydrogen quota leaked %s", private)
		}
	}
	return report
}

func TestHydrogenQuotaNativeHTTPColdReadAndLegacyArgonIsolation(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, anti := range []bool{false, true} {
				name := protocol + map[bool]string{true: "/sse", false: "/json"}[stream] + map[bool]string{true: "/anti", false: "/ordinary"}[anti]
				t.Run(name, func(t *testing.T) {
					scenario := "measured"
					if anti {
						scenario = "atomic_anti"
					}
					service, preparer := newOutputHTTPFixture(t, aistudio.ChannelPlayground, scenario, "gemini-4-hydrogen")
					path := filepath.Join(t.TempDir(), "quota.db")
					store := completeHydrogenQuotaFixture(t, path)
					defer store.Close()
					service.quotaObserver = store.RecordQuotaAttempt
					view := &quotaHTTPView{store: store, account: service.account}
					handler := api.NewHandler(service, api.Config{Admin: &ledgerFixtureAdmin{store: store}, Quota: view})
					requested := "playground/gemini-4-hydrogen"
					if anti {
						requested = "playground/anti-truncation/gemini-4-hydrogen"
					}
					rec := quotaHTTPGenerate(t, handler, requested, protocol, stream)
					if rec.Code != 200 || !strings.Contains(rec.Body.String(), "visible") {
						t.Fatalf("generation %d %s", rec.Code, rec.Body.String())
					}
					report := hydrogenQuotaHTTPRead(t, handler)
					if report.Tokens.Used == nil || *report.Tokens.Used != 1520 || report.Requests.Used == nil || *report.Requests.Used != 1 || report.Tokens.Remaining == nil || *report.Tokens.Remaining != 30000000-1520 || preparer.calls != 1 {
						t.Fatalf("native Hydrogen observation: %+v sends=%d", report, preparer.calls)
					}
					argon := quotaHTTPRead(t, handler)
					if argon.Model != "gemini-4-argon" || argon.Requests.Used == nil || *argon.Requests.Used != 0 {
						t.Fatalf("legacy Argon polluted: %+v", argon)
					}
					carbon := carbonQuotaHTTPRead(t, handler)
					if carbon.Requests.Used == nil || *carbon.Requests.Used != 0 {
						t.Fatalf("Carbon polluted: %+v", carbon)
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
					cold := hydrogenQuotaHTTPRead(t, api.NewHandler(service, api.Config{Quota: view}))
					if cold.Requests.Used == nil || *cold.Requests.Used != 1 || cold.Tokens.Used == nil || *cold.Tokens.Used != 1520 {
						t.Fatalf("Hydrogen cold read: %+v", cold)
					}
				})
			}
		}
	}
}

func TestHydrogenQuotaHTTPRejectsUnknownAndAmbiguousScope(t *testing.T) {
	store := completeHydrogenQuotaFixture(t, filepath.Join(t.TempDir(), "quota.db"))
	defer store.Close()
	handler := api.NewHandler(nil, api.Config{Quota: &quotaHTTPView{store: store, account: "fixture"}})
	for _, query := range []string{"model=gemini-3.8-flash", "model=build/gemini-4-hydrogen", "model=gemini-4-hydrogen&model=gemini-4-argon"} {
		req := httptest.NewRequest("GET", "http://localhost/api/usage/quota?"+query, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("query %s status %d", query, rec.Code)
		}
	}
}
