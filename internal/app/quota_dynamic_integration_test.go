package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

type dynamicQuotaAdmin struct {
	api.AdminService
	models   []aistudio.Model
	accounts []api.AdminAccount
}

func (a dynamicQuotaAdmin) CatalogModels(context.Context) ([]aistudio.Model, error) {
	return a.models, nil
}
func (a dynamicQuotaAdmin) Accounts(context.Context) ([]api.AdminAccount, error) {
	return a.accounts, nil
}

func TestDynamicQuotaUsesCatalogAndPerAccountPlaygroundModel(t *testing.T) {
	const model = "gemini-4-neon-preview"
	const buildOnly = "gemini-4-build-fixture"
	db := completeQuotaFixture(t, filepath.Join(t.TempDir(), "quota.db"))
	defer db.Close()
	admin := dynamicQuotaAdmin{
		models: []aistudio.Model{
			{ID: "playground/anti-truncation/" + model, BaseModel: "models/" + model, Channels: []string{"playground"}, Methods: []string{"GenerateContent"}},
			{ID: buildOnly, Channels: []string{"build"}, Methods: []string{"generateContent"}},
		},
		accounts: []api.AdminAccount{
			{ID: "pro", Enabled: true, State: "ready", BenefitTier: "Pro", Models: []string{"models/" + model}},
			{ID: "unrelated", Enabled: true, State: "ready", BenefitTier: "Ultra", Models: []string{"gemini-4-carbon"}},
			{ID: "blocked", Enabled: true, State: "auth_required", BenefitTier: "Ultra", Models: []string{model}, BlockCode: "ACCOUNT_VERIFICATION_REQUIRED"},
		},
	}
	manager := &runtimeManager{current: &runtimeGeneration{admin: admin}, ledger: db}
	h := api.NewHandler(nil, api.Config{Quota: manager})
	fetch := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "http://localhost/api/usage/quota"+query, nil)
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, name := range []string{buildOnly, "gemini-4-unregistered", model + "-anti"} {
		if v := fetch("?model=" + name); v.Code != http.StatusBadRequest {
			t.Fatalf("non-PG/unregistered/alias model accepted: %s %d", name, v.Code)
		}
	}
	var result api.QuotaReport
	v := fetch("?model=" + model)
	if v.Code != 200 || json.Unmarshal(v.Body.Bytes(), &result) != nil || result.Model != model {
		t.Fatal(v.Code, v.Body.String())
	}
	if result.Tokens.Limit == nil || *result.Tokens.Limit != 15_000_000 || result.Tokens.Remaining == nil || *result.Tokens.Remaining != 15_000_000 || len(result.Accounts) != 2 || result.ExcludedAccounts != 1 {
		t.Fatalf("wrong model membership/estimate: %+v", result)
	}
	var legacy api.QuotaReport
	v = fetch("")
	if v.Code != 200 || json.Unmarshal(v.Body.Bytes(), &legacy) != nil || legacy.Model != "gemini-4-argon" {
		t.Fatal("legacy default changed", v.Code, v.Body.String())
	}
}
