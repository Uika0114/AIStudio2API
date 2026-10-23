package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type legacyChromeProbe struct {
	AdminService
	discoveries, imports int
	input                ChromeImportInput
}

func (p *legacyChromeProbe) ChromeImportProfiles(context.Context) ([]ChromeImportProfile, error) {
	p.discoveries++
	return []ChromeImportProfile{}, nil
}
func (p *legacyChromeProbe) ImportChromeAccounts(_ context.Context, in ChromeImportInput) ([]AdminAccount, error) {
	p.imports++
	p.input = in
	return []AdminAccount{{ID: "fixture@example.com"}}, nil
}

type sourceChromeProbe struct {
	*legacyChromeProbe
	roots []string
}

func (p *sourceChromeProbe) ChromeImportProfilesFrom(_ context.Context, root string) (ChromeImportDiscovery, error) {
	p.roots = append(p.roots, root)
	resolved := root
	if resolved == "" {
		resolved = "C:/FixtureDefault"
	}
	return ChromeImportDiscovery{ChromeRoot: resolved, Profiles: []ChromeImportProfile{{ID: "opaque-source-selection", ChromeRoot: resolved, Profile: "Default", Email: "fixture@example.com", Importable: true, TokenFormat: "v10"}}}, nil
}
func chromeTestMux(admin AdminService) *http.ServeMux {
	s := &server{config: Config{Admin: admin}}
	mux := http.NewServeMux()
	s.registerAdmin(mux)
	return mux
}
func chromeTestRequest(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestChromeSourceRoutesFailClosedForLegacyService(t *testing.T) {
	p := &legacyChromeProbe{}
	mux := chromeTestMux(p)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/api/accounts/import/chrome/source?chrome_root=C%3A%2FFixture"
		w := chromeTestRequest(mux, method, path, `{"chrome_root":"C:/Fixture","account_ids":["opaque"]}`)
		if w.Code != http.StatusNotImplemented || p.discoveries != 0 || p.imports != 0 {
			t.Fatal("source route fell back to legacy browser discovery")
		}
	}
	w := chromeTestRequest(mux, http.MethodPost, "/api/accounts/import/chrome", `{"chrome_root":"C:/Fixture","account_ids":["opaque"]}`)
	if w.Code != http.StatusNotImplemented || p.imports != 0 {
		t.Fatal("legacy service ignored explicit root")
	}
	if w := chromeTestRequest(mux, http.MethodGet, "/api/accounts/import/chrome", ""); w.Code != http.StatusOK || p.discoveries != 1 {
		t.Fatal("legacy default discovery compatibility changed")
	}
	if w := chromeTestRequest(mux, http.MethodPost, "/api/accounts/import/chrome", `{"account_ids":["Default/111"]}`); w.Code != http.StatusCreated || p.imports != 1 {
		t.Fatal("legacy default import compatibility changed")
	}
}

func TestChromeSourceRootEchoAndStrictInputs(t *testing.T) {
	p := &sourceChromeProbe{legacyChromeProbe: &legacyChromeProbe{}}
	mux := chromeTestMux(p)
	for _, query := range []string{"", "?chrome_root=A&chrome_root=B"} {
		if w := chromeTestRequest(mux, http.MethodGet, "/api/accounts/import/chrome/source"+query, ""); w.Code != http.StatusBadRequest || len(p.roots) != 0 {
			t.Fatal("ambiguous discovery touched a browser")
		}
	}
	root := "C:/Fixture Root #2/专用"
	w := chromeTestRequest(mux, http.MethodGet, "/api/accounts/import/chrome/source?"+url.Values{"chrome_root": []string{root}}.Encode(), "")
	var result ChromeImportDiscovery
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ChromeRoot != root || result.Profiles[0].ChromeRoot != root || len(p.roots) != 1 || p.roots[0] != root || p.discoveries != 0 {
		t.Fatal("root context was not preserved on discovery")
	}
	w = chromeTestRequest(mux, http.MethodGet, "/api/accounts/import/chrome/source?chrome_root=", "")
	if w.Code != http.StatusOK || len(p.roots) != 2 || p.roots[1] != "" {
		t.Fatal("explicit default selection was not honored")
	}
	for _, body := range []string{`{"account_ids":["opaque"]}`, `{"chrome_root":" ","account_ids":["opaque"]}`, `{"chrome_root":"C:/Fixture","account_ids":["opaque"],"rebind_account_id":"fixture@example.com"}`} {
		if w := chromeTestRequest(mux, http.MethodPost, "/api/accounts/import/chrome/source", body); w.Code != http.StatusBadRequest || p.imports != 0 {
			t.Fatal("incomplete source import reached importer")
		}
	}
	body := `{"chrome_root":"C:/Fixture","account_ids":["opaque"],"rebind_account_id":"fixture@example.com","confirm_rebind":true}`
	w = chromeTestRequest(mux, http.MethodPost, "/api/accounts/import/chrome/source", body)
	if w.Code != http.StatusOK || p.imports != 1 || p.input.ChromeRoot != "C:/Fixture" || !p.input.ConfirmRebind || p.input.RebindAccountID != "fixture@example.com" {
		t.Fatal("explicit rebinding contract was not delivered")
	}
}
