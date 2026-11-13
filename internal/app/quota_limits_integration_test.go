package app

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

func TestQuotaLimitsNativeHTTPObservationAndColdRead(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-4-carbon", "gemini-4-hydrogen"} {
		for _, protocol := range []string{"chat", "responses", "gemini"} {
			t.Run(model+"/"+protocol, func(t *testing.T) {
				service, preparer := newOutputHTTPFixture(t, aistudio.ChannelPlayground, "http429", model)
				preparer.tierHeader = "TIER2"
				preparer.quotaMessage = fmt.Sprintf("private-error-sentinel\nQuota exceeded for metric: generativelanguage.googleapis.com/generate_content_tokens_per_model_per_user, limit: 19000000, model: %s, description: Generate content tokens per user per day limit exceeded.\nQuota exceeded for metric: generativelanguage.googleapis.com/generate_requests_per_model_per_user, limit: 150, model: %s, description: Generate content requests per user per day limit exceeded.", model, model)
				path := filepath.Join(t.TempDir(), "limits.db")
				store := completeQuotaFixture(t, path)
				defer store.Close()
				service.quotaObserver = store.RecordQuotaAttempt
				view := &quotaHTTPView{store: store, account: service.account}
				handler := api.NewHandler(service, api.Config{Quota: view})
				response := quotaHTTPGenerate(t, handler, "playground/"+model, protocol, false)
				if response.Code != 429 || preparer.calls != 1 {
					t.Fatalf("error path changed: status=%d sends=%d", response.Code, preparer.calls)
				}
				read := func() api.QuotaReport {
					request := httptest.NewRequest("GET", "http://localhost/api/usage/quota?model="+model, nil)
					request.RemoteAddr = "127.0.0.1:12345"
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					var report api.QuotaReport
					if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &report) != nil {
						t.Fatalf("quota read: %d %s", response.Code, response.Body.String())
					}
					for _, forbidden := range []string{"private-error-sentinel", "synthetic-not-a-credential", "Quota exceeded", "X-AIStudio-G1-Tier", "quota_limits", "quota_tier"} {
						if strings.Contains(response.Body.String(), forbidden) {
							t.Fatalf("quota report leaked %s", forbidden)
						}
					}
					return report
				}
				assert := func(report api.QuotaReport) {
					if report.Tokens.Limit == nil || *report.Tokens.Limit != 19000000 ||
						report.Requests.Limit == nil || *report.Requests.Limit != 150 ||
						report.Requests.Used == nil || *report.Requests.Used != 1 ||
						report.Tokens.Used == nil || *report.Tokens.Used != 0 {
						t.Fatalf("native limit observation missing: %+v", report)
					}
				}
				assert(read())
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := requestdb.Open(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				view.store = reopened
				assert(read())
			})
		}
	}
}

func TestQuotaLimitsHTTPTokenErrorDoesNotExhaustRequests(t *testing.T) {
	service, preparer := newOutputHTTPFixture(t, aistudio.ChannelPlayground, "http429", "gemini-4-argon")
	preparer.tierHeader = "TIER2"
	preparer.quotaMessage = "Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_tokens_per_model_per_user, limit: 19000000, model: gemini-4-argon, description: Generate content tokens per user per day limit exceeded."
	store := completeQuotaFixture(t, filepath.Join(t.TempDir(), "tokens-only.db"))
	defer store.Close()
	service.quotaObserver = store.RecordQuotaAttempt
	handler := api.NewHandler(service, api.Config{Quota: &quotaHTTPView{store: store, account: service.account}})
	response := quotaHTTPGenerate(t, handler, "playground/gemini-4-argon", "chat", false)
	if response.Code != 429 || preparer.calls != 1 {
		t.Fatalf("error path changed: status=%d sends=%d", response.Code, preparer.calls)
	}
	report := quotaHTTPRead(t, handler)
	if report.Tokens.Remaining == nil || *report.Tokens.Remaining != 0 ||
		report.Requests.Remaining == nil || *report.Requests.Remaining != 199 {
		t.Fatalf("HTTP evidence did not refine the initial unknown 429: %+v", report)
	}
}
