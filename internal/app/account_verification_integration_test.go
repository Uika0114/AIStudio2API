package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func TestVerificationBlockAdminQuotaAndExplicitClear(t *testing.T) {
	f := newQueryAppFixture(t, "new", "reauth", false)
	lease, _ := aistudio.AccountLeaseFromContext(f.ctx)
	if err := f.pool.SetCatalog(f.account.ID, aistudio.BenefitTierUltra, f.account.Models); err != nil {
		t.Fatal(err)
	}
	if err := lease.MarkAccountVerificationRequired(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	store := completeQuotaFixture(t, filepath.Join(t.TempDir(), "verification-quota.db"))
	defer store.Close()
	manager := &runtimeManager{current: &runtimeGeneration{admin: &runtimeAdmin{pool: f.pool}}, ledger: store}
	h := api.NewHandler(nil, api.Config{Admin: manager, Quota: manager})
	id := f.pool.Status()[0].VerificationID
	if id == "" {
		t.Fatal("missing verification ID")
	}
	report := quotaHTTPRead(t, h)
	if len(report.Accounts) != 1 || report.Accounts[0].Included || report.Accounts[0].BlockCode != aistudio.AccountVerificationRequiredCode || report.Tokens.Limit != nil {
		t.Fatalf("blocked account was not excluded: %+v", report)
	}
	path := "/api/accounts/" + f.account.ID + "/verification/clear"
	locked := api.NewHandler(nil, api.Config{Admin: manager, Quota: manager, AdminAuthEnabled: true, AdminUsername: "fixture", AdminPassword: "fixture-password"})
	anon := httptest.NewRequest(http.MethodPost, "http://localhost"+path, strings.NewReader(`{"verification_id":"`+id+`"}`))
	anon.RemoteAddr = "127.0.0.1:12345"
	anonymous := httptest.NewRecorder()
	locked.ServeHTTP(anonymous, anon)
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous verification clear: %d", anonymous.Code)
	}
	clear := func(id string, crossSite bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://localhost"+path, strings.NewReader(`{"verification_id":"`+id+`"}`))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		if crossSite {
			r.Header.Set("Origin", "https://not-the-admin.invalid")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if r := clear(id, true); r.Code != http.StatusForbidden {
		t.Fatalf("cross-origin clear: %d %s", r.Code, r.Body.String())
	}
	if r := clear("older-verification-id", false); r.Code != http.StatusConflict {
		t.Fatalf("stale verification clear: %d %s", r.Code, r.Body.String())
	}
	if r := clear(id, false); r.Code != http.StatusNoContent {
		t.Fatalf("verified-account clear: %d %s", r.Code, r.Body.String())
	}
	report = quotaHTTPRead(t, h)
	if len(report.Accounts) != 1 || !report.Accounts[0].Included || report.Accounts[0].BlockCode != "" || report.Tokens.Limit == nil {
		t.Fatalf("eligible account did not return to quota pool: %+v", report)
	}
	if f.refresh.Load() != 0 || f.auth.Load() != 0 || f.ack.Load() != 0 {
		t.Fatal("operator confirmation initiated upstream authentication")
	}
}
