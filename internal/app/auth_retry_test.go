package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

func fixtureRefreshCookies(value string) []aistudio.StateCookie {
	var cookies []aistudio.StateCookie
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, aistudio.StateCookie{Name: name, Value: value, Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "Lax"})
	}
	return cookies
}

func fixtureChromeSource(t *testing.T) aistudio.AuthSource {
	t.Helper()
	return aistudio.AuthSource{Browser: "chrome", ChromeRoot: filepath.Join(t.TempDir(), "unmounted-root"), Profile: "Default", GaiaID: "111", Email: "fixture@example.com"}
}

func fixtureAuthRefresher(t *testing.T, extension *aistudio.AuthExtension) (*authRuntimeRefresher, context.Context, string) {
	t.Helper()
	directory := t.TempDir()
	storage := filepath.Join(directory, "storage-state.json")
	state := aistudio.StorageState{Cookies: fixtureRefreshCookies("fixture-old"), Origins: []aistudio.StorageOrigin{}}
	if extension != nil {
		if err := state.SetAuthExtension(*extension); err != nil {
			t.Fatal(err)
		}
	}
	if err := aistudio.WriteStorageState(storage, state); err != nil {
		t.Fatal(err)
	}
	account := &aistudio.Account{ID: "fixture@example.com", Directory: directory, StoragePath: storage, RuntimePath: filepath.Join(directory, "runtime-state.json"), StorageState: state, Config: aistudio.DefaultAccountConfig("fixture@example.com")}
	pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
	lease, err := pool.AcquireAccount(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Release() })
	refresher := &authRuntimeRefresher{pool: pool, requests: newRequestRegistry(t.Context()), reset: func(string) error { return nil }, prepareHeaders: func(string) (func(bool), error) { return func(bool) {}, nil }}
	return refresher, aistudio.ContextWithAccountLease(t.Context(), lease), storage
}

func assertFixtureStateUnchanged(t *testing.T, storage string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(storage)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed refresh replaced original fixture state")
	}
}

func TestLegacyAuthWithoutSourceRequiresExplicitBinding(t *testing.T) {
	r, ctx, storage := fixtureAuthRefresher(t, nil)
	before, _ := os.ReadFile(storage)
	r.importCurrent = func(context.Context, aistudio.AuthSource, *aistudio.ChromeOAuthMaterial, string) (aistudio.StorageState, error) {
		t.Fatal("legacy account scanned a browser")
		return aistudio.StorageState{}, nil
	}
	if r.Available(ctx) {
		t.Fatal("source-less account was eligible for implicit discovery")
	}
	if err := r.Refresh(ctx); !errors.Is(err, chromeauth.ErrSourceBindingRequired) {
		t.Fatal("legacy account must request source binding")
	}
	assertFixtureStateUnchanged(t, storage, before)
}

func TestNormalAuthRefreshNeverReadsChrome(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound-unmounted-root", true: "legacy-rootless"}[legacy], func(t *testing.T) {
			source := fixtureChromeSource(t)
			if legacy {
				source.ChromeRoot = ""
				source.GaiaID = ""
			}
			ext := aistudio.AuthExtension{Source: source, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: "111", RefreshToken: "fixture-old-token", WrappedBindingKey: []byte("fixture-binding")}}
			r, ctx, storage := fixtureAuthRefresher(t, &ext)
			calls := 0
			r.refresh = func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
				calls++
				return fixtureRefreshCookies("fixture-new"), nil
			}
			r.importCurrent = func(context.Context, aistudio.AuthSource, *aistudio.ChromeOAuthMaterial, string) (aistudio.StorageState, error) {
				t.Fatal("normal refresh read Chrome")
				return aistudio.StorageState{}, nil
			}
			if !r.Available(ctx) {
				t.Fatal("saved material became unavailable")
			}
			if err := r.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := aistudio.LoadStorageState(storage)
			if err != nil {
				t.Fatal(err)
			}
			kept, _, _ := stored.AuthExtension()
			if calls != 1 || kept.Source.ChromeRoot != source.ChromeRoot || stored.Cookies[0].Value != "fixture-new" {
				t.Fatal("wrong saved-material refresh result")
			}
		})
	}
}

func TestAuthSourceRecoveryAndFailureAtomicity(t *testing.T) {
	for _, kind := range []string{"success", "rootless", "network-error", "wrong-root", "wrong-profile", "wrong-gaia", "prepare-failure", "cancelled", "cancel-after-exchange"} {
		t.Run(kind, func(t *testing.T) {
			source := fixtureChromeSource(t)
			if kind == "rootless" {
				source.ChromeRoot = ""
			}
			ext := aistudio.AuthExtension{Source: source, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: "111", RefreshToken: "fixture-old-token", WrappedBindingKey: []byte("fixture-binding")}}
			r, ctx, storage := fixtureAuthRefresher(t, &ext)
			before, _ := os.ReadFile(storage)
			imports, resets := 0, 0
			child, cancel := context.WithCancel(ctx)
			defer cancel()
			ctx = child
			r.refresh = func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
				if kind == "network-error" {
					return nil, errors.New("fixture network unavailable")
				}
				return nil, chromeauth.ErrCredentialsRejected
			}
			r.reset = func(string) error { resets++; return nil }
			if kind == "prepare-failure" {
				r.prepareHeaders = func(string) (func(bool), error) { return nil, errors.New("fixture prepare failure") }
			}
			r.importCurrent = func(_ context.Context, s aistudio.AuthSource, m *aistudio.ChromeOAuthMaterial, _ string) (aistudio.StorageState, error) {
				imports++
				if !chromeauth.SameSource(s, source) || m.GaiaID != s.GaiaID {
					t.Fatal("recovery selected another source")
				}
				updated := aistudio.StorageState{Cookies: fixtureRefreshCookies("fixture-chrome")}
				switch kind {
				case "wrong-root":
					s.ChromeRoot = filepath.Join(s.ChromeRoot, "other")
				case "wrong-profile":
					s.Profile = "Profile 2"
				case "wrong-gaia":
					s.GaiaID = "222"
				}
				if err := updated.SetAuthExtension(aistudio.AuthExtension{Source: s, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: s.GaiaID, RefreshToken: "fixture-new-token", WrappedBindingKey: []byte("fixture-new-binding")}}); err != nil {
					t.Fatal(err)
				}
				if kind == "cancel-after-exchange" {
					cancel()
				}
				return updated, nil
			}
			if kind == "cancelled" {
				cancel()
			}
			err := r.Refresh(ctx)
			if kind == "success" {
				if err != nil || imports != 1 || resets != 1 {
					t.Fatal("exact source recovery failed")
				}
				stored, e := aistudio.LoadStorageState(storage)
				if e != nil {
					t.Fatal(e)
				}
				kept, ok, e := stored.AuthExtension()
				if e != nil || !ok || !chromeauth.SameSource(kept.Source, source) || kept.OAuth.RefreshToken != "fixture-new-token" {
					t.Fatal("lost source or renewed material")
				}
				login := aistudio.StorageState{Cookies: fixtureRefreshCookies("fixture-login")}
				if e := keepAuthExtension(&login, stored); e != nil {
					t.Fatal(e)
				}
				kept, ok, e = login.AuthExtension()
				if e != nil || !ok || !chromeauth.SameSource(kept.Source, source) {
					t.Fatal("relogin discarded precise source")
				}
				return
			}
			if err == nil {
				t.Fatal("failed recovery reported success")
			}
			if (kind == "rootless" || kind == "network-error" || kind == "cancelled") && imports != 0 {
				t.Fatal("failure invoked source import")
			}
			if kind == "rootless" && !errors.Is(err, chromeauth.ErrSourceBindingRequired) {
				t.Fatal("missing source did not request binding")
			}
			if kind != "prepare-failure" && resets != 0 {
				t.Fatal("invalid recovery reset runtime")
			}
			assertFixtureStateUnchanged(t, storage, before)
		})
	}
}

func TestKnownSourceWithoutOAuthCanRecover(t *testing.T) {
	source := fixtureChromeSource(t)
	r, ctx, storage := fixtureAuthRefresher(t, &aistudio.AuthExtension{Source: source})
	r.importCurrent = func(_ context.Context, s aistudio.AuthSource, _ *aistudio.ChromeOAuthMaterial, _ string) (aistudio.StorageState, error) {
		if !chromeauth.SameSource(s, source) {
			t.Fatal("source identity changed")
		}
		state := aistudio.StorageState{Cookies: fixtureRefreshCookies("fixture-import")}
		err := state.SetAuthExtension(aistudio.AuthExtension{Source: s, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: s.GaiaID, RefreshToken: "fixture-token", WrappedBindingKey: []byte("fixture-binding")}})
		return state, err
	}
	if !r.Available(ctx) {
		t.Fatal("complete bound source unavailable")
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := aistudio.LoadStorageState(storage)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, _ := stored.AuthExtension()
	if !chromeauth.SameSource(kept.Source, source) {
		t.Fatal("source was not persisted")
	}
}
