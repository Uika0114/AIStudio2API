package authhelper

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

// Every credential-like value in these fixtures is synthetic. No Chrome or Google is used.
func projectionFullState(t *testing.T, m Mapping, value string, outside int) aistudio.StorageState {
	t.Helper()
	s := aistudio.StorageState{Cookies: fixtureCookies(value), Origins: []aistudio.StorageOrigin{{Origin: "https://accounts.google.com", LocalStorage: []aistudio.StorageItem{{Name: "fixture", Value: "ORIGIN_SENTINEL"}}}}}
	s.Cookies = append(s.Cookies, aistudio.StateCookie{Name: "AISTUDIO_PREF", Value: "in-domain-extra", Domain: "aistudio.google.com", Path: "/fixture", Expires: -1, HTTPOnly: true, Secure: true, SameSite: "Lax"})
	for i := range outside {
		s.Cookies = append(s.Cookies, aistudio.StateCookie{Name: fmt.Sprintf("OTHER_%03d", i), Value: "OUTSIDE_SENTINEL", Domain: ".youtube.com", Path: "/", Expires: -1, Secure: true, SameSite: "None"})
	}
	ext := aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", ChromeRoot: filepath.Join(t.TempDir(), "synthetic-source"), Profile: "Default", GaiaID: m.GaiaID, Email: m.Email}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: m.GaiaID, RefreshToken: "OAUTH_SENTINEL", WrappedBindingKey: []byte("KEY_SENTINEL")}}
	if err := s.SetAuthExtension(ext); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["fixture_unknown"] = json.RawMessage(`{"nested":[1,"UNKNOWN_SENTINEL"],"keep":true}`)
	data, err = json.Marshal(fields)
	if err != nil || json.Unmarshal(data, &s) != nil {
		t.Fatal("synthetic extension setup failed", err)
	}
	return s
}

func assertProjectionFullState(t *testing.T, want, got aistudio.StorageState) {
	t.Helper()
	we, _, err := want.AuthExtension()
	if err != nil {
		t.Fatal(err)
	}
	ge, _, err := got.AuthExtension()
	if err != nil || !reflect.DeepEqual(want.Cookies, got.Cookies) || !reflect.DeepEqual(want.Origins, got.Origins) || !reflect.DeepEqual(we, ge) {
		t.Fatal("complete cookies, origins or native material changed", err)
	}
	unknown := func(s aistudio.StorageState) json.RawMessage {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		return fields["fixture_unknown"]
	}
	if !reflect.DeepEqual(unknown(want), unknown(got)) {
		t.Fatal("root unknown extension changed")
	}
}

func TestCookieProjectionRegisterPreservesCompleteAuthority(t *testing.T) {
	_, m, _ := fixtureBroker(t, nil)
	m.StateFile = filepath.Join(t.TempDir(), "unregistered", "storage-state.json")
	full := projectionFullState(t, m, "register-fixture", 140)
	if err := full.Validate(); err != nil {
		t.Fatal("native full state is invalid", err)
	}
	if safeCode(ValidateCookies(full.Cookies, m.CookieDomains)) != "COOKIE_POLICY_FAILURE" {
		t.Fatal("strict receiver accepted complete multi-domain state")
	}
	if err := aistudio.WriteStorageState(m.StateFile, full); err != nil {
		t.Fatal(err)
	}
	if err := registerAuthority(t.Context(), m, "S-1-5-21-fixture-only"); err != nil {
		t.Fatal("multi-domain native authority registration rejected", err)
	}
	got, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectionFullState(t, full, got)
	meta, err := got.Session()
	if err != nil || meta.Authority.Enabled || meta.Authority.Epoch != 1 || meta.Authority.CookieRevision != 0 || meta.Authority.MaterialVersion != 1 || meta.Authority.Operation != nil || meta.Authority.WindowsUserSID != "S-1-5-21-fixture-only" {
		t.Fatal("registration changed authority contract", err)
	}
}

func TestCookieProjectionBootstrapPreservesCompleteAuthority(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	full := projectionFullState(t, m, "bootstrap-fixture", 140)
	old, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := old.Session()
	if err != nil || full.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &full) != nil {
		t.Fatal("authority fixture save", err)
	}
	req := fixtureRequest(m, "projection_bootstrap_1", 0)
	req.Reason = "bootstrap"
	response, err := b.Refresh(t.Context(), m.Consumer, req)
	if err != nil {
		t.Fatal("multi-domain bootstrap rejected", err)
	}
	if count.Load() != 0 || response.CookieRevision != 1 || response.MaterialVersion != 1 || !reflect.DeepEqual(response.Cookies, full.Cookies[:4]) {
		t.Fatal("bootstrap authenticated or did not send the exact subset")
	}
	got, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectionFullState(t, full, got)
	ackResult(t, b, m, response)
}

func TestCookieProjectionNativeRenewStillReturnsCompleteCookies(t *testing.T) {
	_, m, _ := fixtureBroker(t, nil)
	state := projectionFullState(t, m, "native-fixture", 2)
	want := projectionFullState(t, m, "renewed-native-fixture", 3).Cookies
	refreshes := 0
	got, err := nativeRenewWith(t.Context(), state, m, false, func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
		refreshes++
		return want, nil
	}, func(context.Context, chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
		t.Fatal("saved material unexpectedly read Chrome")
		return nil, nil
	})
	if err != nil || refreshes != 1 || !reflect.DeepEqual(got.Cookies, want) || got.Validate() != nil {
		t.Fatal("native renewal filtered complete output", err)
	}
	if ValidateCookies(got.Cookies, nil) == nil {
		t.Fatal("native full output incorrectly satisfies the strict remote contract")
	}
}
