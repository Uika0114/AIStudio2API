package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

// runServer exposes runtimeManager as both Service and Admin. Test that exact
// outer facade, not only the inner runtimeAdmin or a source-aware API fixture.
func managerChromeHandler(t *testing.T, admin api.AdminService) (*runtimeManager, http.Handler) {
	t.Helper()
	manager := &runtimeManager{requests: newRequestRegistry(t.Context()), current: &runtimeGeneration{admin: admin}}
	return manager, api.NewHandler(manager, api.Config{Admin: manager})
}

func managerChromeRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:43900"
	request.Host = "127.0.0.1:2048"
	return request
}

func TestChromeManagerHTTPSourceDiscoveryAndImport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dedicated")
	row := fixtureChromeRow(t, root, "fixture@example.invalid", "Default", "111")
	provider := &fixtureChromeProvider{discover: func(got string) ([]chromeauth.Account, error) {
		if got != row.ChromeRoot {
			t.Fatal("facade lost the selected source root")
		}
		return []chromeauth.Account{row}, nil
	}, importAccounts: func(_ context.Context, options chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
		if options.ChromeRoot != row.ChromeRoot || len(options.AccountIDs) != 1 || options.AccountIDs[0] != row.ID {
			t.Fatal("source import lost exact selection")
		}
		return []chromeauth.ImportResult{fixtureChromeResult(t, row)}, nil
	}}
	saved := 0
	admin := &runtimeAdmin{chrome: provider, pool: aistudio.NewAccountPool(nil, 1), requests: newRequestRegistry(t.Context())}
	admin.chromeCreate = func(_ context.Context, cfg aistudio.AccountConfig, state aistudio.StorageState) (api.AdminAccount, error) {
		saved++
		extension, exists, err := state.AuthExtension()
		if err != nil || !exists || extension.Source.ChromeRoot != row.ChromeRoot || extension.Source.Profile != row.Profile || extension.Source.GaiaID != row.GaiaID {
			t.Fatal("saved source identity drifted")
		}
		return api.AdminAccount{ID: row.Email, Label: cfg.Label}, nil
	}
	_, handler := managerChromeHandler(t, admin)
	target := "/api/accounts/import/chrome/source?" + url.Values{"chrome_root": {root}}.Encode()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, managerChromeRequest(http.MethodGet, target, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("real manager discovery returned %d: %s", response.Code, response.Body.String())
	}
	var discovery api.ChromeImportDiscovery
	if err := json.Unmarshal(response.Body.Bytes(), &discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.ChromeRoot != row.ChromeRoot || len(discovery.Profiles) != 1 || discovery.Profiles[0].ID != row.ID || provider.discoveries != 1 || provider.imports != 0 {
		t.Fatal("manager discovery did not preserve source identity")
	}
	for _, secret := range []string{"cookies", "refresh_token", "wrapped_binding_key"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("discovery exposed auth material")
		}
	}

	payload, _ := json.Marshal(api.ChromeImportInput{ChromeRoot: root, AccountIDs: []string{row.ID}})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, managerChromeRequest(http.MethodPost, "/api/accounts/import/chrome/source", string(payload)))
	if response.Code != http.StatusCreated || provider.imports != 1 || saved != 1 {
		t.Fatalf("real manager selected import failed: status=%d imports=%d saves=%d body=%s", response.Code, provider.imports, saved, response.Body.String())
	}
}

func TestChromeManagerHTTPSourceUsesCurrentGeneration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dedicated")
	first := fixtureChromeRow(t, root, "first@example.invalid", "Default", "111")
	second := fixtureChromeRow(t, root, "second@example.invalid", "Profile 2", "222")
	fixture := func(row chromeauth.Account) *runtimeAdmin {
		return &runtimeAdmin{pool: aistudio.NewAccountPool(nil, 1), chrome: &fixtureChromeProvider{discover: func(got string) ([]chromeauth.Account, error) {
			if got != row.ChromeRoot {
				t.Fatal("generation switch changed source root")
			}
			return []chromeauth.Account{row}, nil
		}}}
	}
	manager, handler := managerChromeHandler(t, fixture(first))
	target := "/api/accounts/import/chrome/source?" + url.Values{"chrome_root": {root}}.Encode()
	for _, row := range []chromeauth.Account{first, second} {
		manager.mu.Lock()
		manager.current = &runtimeGeneration{admin: fixture(row)}
		manager.mu.Unlock()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, managerChromeRequest(http.MethodGet, target, ""))
		var discovery api.ChromeImportDiscovery
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &discovery) != nil || len(discovery.Profiles) != 1 || discovery.Profiles[0].ID != row.ID {
			t.Fatal("facade used stale generation")
		}
	}
}

type managerLegacyChromeAdmin struct {
	api.AdminService
	calls int
}

func (a *managerLegacyChromeAdmin) ChromeImportProfiles(context.Context) ([]api.ChromeImportProfile, error) {
	a.calls++
	return nil, nil
}
func TestChromeManagerHTTPSourceNeverFallsBackToDefault(t *testing.T) {
	legacy := &managerLegacyChromeAdmin{}
	_, handler := managerChromeHandler(t, legacy)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, managerChromeRequest(http.MethodGet, "/api/accounts/import/chrome/source?chrome_root="+url.QueryEscape(filepath.Join(t.TempDir(), "selected")), ""))
	if response.Code != http.StatusNotImplemented || legacy.calls != 0 {
		t.Fatal("unsupported inner runtime fell back to default browser")
	}
}
