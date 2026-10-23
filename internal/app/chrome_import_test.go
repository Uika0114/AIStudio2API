package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

type fixtureChromeProvider struct {
	discover             func(string) ([]chromeauth.Account, error)
	importAccounts       func(context.Context, chromeauth.ImportOptions) ([]chromeauth.ImportResult, error)
	discoveries, imports int
}

func (p *fixtureChromeProvider) Discover(root string) ([]chromeauth.Account, error) {
	p.discoveries++
	if p.discover == nil {
		return nil, errors.New("unexpected fixture discovery")
	}
	return p.discover(root)
}
func (p *fixtureChromeProvider) Import(ctx context.Context, o chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
	p.imports++
	if p.importAccounts == nil {
		return nil, errors.New("unexpected fixture exchange")
	}
	return p.importAccounts(ctx, o)
}

func fixtureChromeRow(t *testing.T, root, email, profile, gaia string) chromeauth.Account {
	t.Helper()
	root, err := chromeauth.NormalizeChromeRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	// Consumer IDs are opaque. Native namespace validation has its own driver tests.
	return chromeauth.Account{ID: "opaque-" + root + "/" + profile + "/" + gaia, ChromeRoot: root, Email: email, Profile: profile, GaiaID: gaia, DisplayName: profile, Locale: "en-US", Importable: true, TokenFormat: "v10"}
}
func fixtureChromeResult(t *testing.T, row chromeauth.Account) chromeauth.ImportResult {
	t.Helper()
	state := aistudio.StorageState{Cookies: fixtureRefreshCookies("fixture-new-cookie")}
	err := state.SetAuthExtension(aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", ChromeRoot: row.ChromeRoot, Profile: row.Profile, GaiaID: row.GaiaID, Email: row.Email}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: row.GaiaID, RefreshToken: "fixture-new-token", WrappedBindingKey: []byte("fixture-wrapped-binding")}})
	if err != nil {
		t.Fatal(err)
	}
	return chromeauth.ImportResult{Email: row.Email, Profile: row.Profile, Locale: row.Locale, State: state}
}

func TestChromeAdminDiscoveryRetainsRootDiagnosticsAndExistingAccounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dedicated")
	row := fixtureChromeRow(t, root, "fixture@example.com", "Default", "111")
	unsupported := fixtureChromeRow(t, root, "other@example.com", "Profile 2", "222")
	unsupported.Importable = false
	unsupported.Reason = "fixture missing binding"
	p := &fixtureChromeProvider{discover: func(got string) ([]chromeauth.Account, error) {
		if got != row.ChromeRoot {
			t.Fatal("discovery visited another root")
		}
		return []chromeauth.Account{row, unsupported}, nil
	}}
	account := &aistudio.Account{ID: row.Email, Config: aistudio.DefaultAccountConfig(row.Email)}
	admin := &runtimeAdmin{chrome: p, pool: aistudio.NewAccountPool([]*aistudio.Account{account}, 1)}
	result, err := admin.ChromeImportProfilesFrom(t.Context(), root)
	if err != nil || result.ChromeRoot != row.ChromeRoot || len(result.Profiles) != 2 || result.Profiles[0].ExistingAccountID != row.Email || result.Profiles[1].Importable || result.Profiles[1].Reason == "" || p.discoveries != 1 || p.imports != 0 {
		t.Fatal("root discovery lost diagnostics or existing identities")
	}
	raw, _ := json.Marshal(result)
	for _, forbidden := range []string{"refresh_token", "wrapped_binding_key", "cookies", "fixture-new-token"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("discovery leaked authentication material")
		}
	}
}

func TestChromeAdminNewImportPreflightAndPersistence(t *testing.T) {
	for _, kind := range []string{"success", "empty-selection", "relative-root", "stale-root-id", "already-exists", "exchange-error", "wrong-result-root", "duplicate-email", "partial-save"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "dedicated")
			row := fixtureChromeRow(t, root, "new@example.com", "Default", "111")
			rows := []chromeauth.Account{row}
			pool := aistudio.NewAccountPool(nil, 1)
			if kind == "already-exists" {
				pool = aistudio.NewAccountPool([]*aistudio.Account{{ID: row.Email, Config: aistudio.DefaultAccountConfig(row.Email)}}, 1)
			}
			if kind == "partial-save" {
				rows = append(rows, fixtureChromeRow(t, root, "second@example.com", "Profile 2", "222"))
			}
			if kind == "duplicate-email" {
				rows = append(rows, fixtureChromeRow(t, root, row.Email, "Profile 2", "111"))
			}
			p := &fixtureChromeProvider{discover: func(got string) ([]chromeauth.Account, error) {
				if got != row.ChromeRoot {
					t.Fatal("preflight read another root")
				}
				return rows, nil
			}}
			p.importAccounts = func(_ context.Context, o chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
				if o.ChromeRoot != row.ChromeRoot || o.Proxy != "http://127.0.0.1:9998" {
					t.Fatal("import changed root or selected egress")
				}
				if kind == "exchange-error" {
					return nil, errors.New("fixture exchange failed")
				}
				var results []chromeauth.ImportResult
				for _, r := range rows {
					if kind == "wrong-result-root" {
						r.ChromeRoot = filepath.Join(r.ChromeRoot, "other")
					}
					results = append(results, fixtureChromeResult(t, r))
				}
				return results, nil
			}
			admin := &runtimeAdmin{pool: pool, requests: newRequestRegistry(t.Context()), chrome: p}
			saves := 0
			storage := filepath.Join(t.TempDir(), "saved-fixture.json")
			admin.chromeCreate = func(_ context.Context, cfg aistudio.AccountConfig, state aistudio.StorageState) (api.AdminAccount, error) {
				saves++
				if kind == "partial-save" && saves == 2 {
					return api.AdminAccount{}, errors.New("fixture save failed")
				}
				if cfg.Proxy != "http://127.0.0.1:9998" {
					t.Fatal("lost new-account egress")
				}
				if err := aistudio.WriteStorageState(storage, state); err != nil {
					return api.AdminAccount{}, err
				}
				return api.AdminAccount{ID: cfg.Label, Label: cfg.Label}, nil
			}
			input := api.ChromeImportInput{ChromeRoot: root, Proxy: "http://127.0.0.1:9998", AccountIDs: []string{row.ID}}
			if kind == "partial-save" || kind == "duplicate-email" {
				input.AccountIDs = append(input.AccountIDs, rows[1].ID)
			}
			if kind == "empty-selection" {
				input.AccountIDs = nil
			}
			if kind == "relative-root" {
				input.ChromeRoot = "relative/root"
			}
			if kind == "stale-root-id" {
				input.AccountIDs = []string{"opaque-another-root/Default/111"}
			}
			created, err := admin.ImportChromeAccounts(t.Context(), input)
			if kind == "success" {
				if err != nil || len(created) != 1 || saves != 1 || p.imports != 1 {
					t.Fatal("new source import failed")
				}
				stored, e := aistudio.LoadStorageState(storage)
				if e != nil {
					t.Fatal(e)
				}
				ext, _, _ := stored.AuthExtension()
				if ext.Source.ChromeRoot != row.ChromeRoot || ext.Source.Profile != row.Profile || ext.Source.GaiaID != row.GaiaID {
					t.Fatal("saved source lost root, profile or Gaia")
				}
				return
			}
			if err == nil {
				t.Fatal("invalid import reported success")
			}
			if kind == "partial-save" {
				if len(created) != 1 || saves != 2 || p.imports != 1 {
					t.Fatal("partial batch was replayed or misreported")
				}
				if _, e := aistudio.LoadStorageState(storage); e != nil {
					t.Fatal("prior batch success was removed")
				}
				return
			}
			if saves != 0 {
				t.Fatal("invalid import persisted a state")
			}
			if kind != "exchange-error" && kind != "wrong-result-root" && p.imports != 0 {
				t.Fatal("preflight failure fetched authentication")
			}
			if (kind == "empty-selection" || kind == "relative-root") && p.discoveries != 0 {
				t.Fatal("invalid request discovered a browser")
			}
		})
	}
}

func TestChromeAdminRebindPreservesAccountAndFailureAtomicity(t *testing.T) {
	for _, kind := range []string{"success", "disabled-success", "first-binding", "unconfirmed", "wrong-target", "wrong-gaia", "wrong-result-root", "exchange-error", "prepare-error", "write-error", "cancelled", "busy", "mixed-environment"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "write-error" && runtime.GOOS != "windows" {
				t.Skip("Windows read-only replacement fixture")
			}
			directory := t.TempDir()
			storage := filepath.Join(directory, "storage-state.json")
			old := aistudio.StorageState{Cookies: fixtureRefreshCookies("fixture-old-cookie")}
			ext := aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", Email: "fixture@example.com", Profile: "Old Profile"}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: "111", RefreshToken: "fixture-old-token", WrappedBindingKey: []byte("fixture-old-binding")}}
			if kind == "wrong-gaia" {
				ext.OAuth.GaiaID = "999"
			}
			if kind == "first-binding" {
				ext.OAuth = nil
			}
			if err := old.SetAuthExtension(ext); err != nil {
				t.Fatal(err)
			}
			if err := aistudio.WriteStorageState(storage, old); err != nil {
				t.Fatal(err)
			}
			cfg := aistudio.DefaultAccountConfig("fixture@example.com")
			cfg.Proxy = "http://127.0.0.1:9998"
			cfg.Locale = "en-US"
			cfg.Timezone = "UTC"
			cfg.Enabled = kind != "disabled-success"
			account := &aistudio.Account{ID: cfg.Label, Directory: directory, StoragePath: storage, RuntimePath: filepath.Join(directory, "runtime-state.json"), Config: cfg, StorageState: old, Models: []aistudio.Model{{Name: "fixture-model"}}, BenefitTier: aistudio.BenefitTierPro}
			pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
			if err := pool.SetCatalog(account.ID, aistudio.BenefitTierPro, []aistudio.Model{{Name: "fixture-model"}}); err != nil {
				t.Fatal(err)
			}
			seed, err := pool.AcquireAccount(t.Context(), account.ID)
			if err != nil {
				t.Fatal(err)
			}
			generation := seed.ModelAccessGeneration()
			if err := seed.Release(); err != nil {
				t.Fatal(err)
			}
			if err := pool.MarkCooldownIfGeneration(account.ID, "", generation, time.Now(), time.Now().Add(time.Hour), "fixture-quota"); err != nil {
				t.Fatal(err)
			}
			beforeCooldowns := pool.Status()[0].Cooldowns
			if len(beforeCooldowns) == 0 {
				t.Fatal("fixture did not seed quota state")
			}
			before, _ := os.ReadFile(storage)
			beforeModels := append([]aistudio.Model(nil), account.Models...)
			row := fixtureChromeRow(t, filepath.Join(t.TempDir(), "dedicated"), account.ID, "Default", "111")
			p := &fixtureChromeProvider{discover: func(got string) ([]chromeauth.Account, error) {
				if got != row.ChromeRoot {
					t.Fatal("rebind visited another root")
				}
				return []chromeauth.Account{row}, nil
			}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p.importAccounts = func(_ context.Context, o chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
				if o.ExactSource == nil || o.ExactSource.ChromeRoot != row.ChromeRoot || o.ExactSource.Profile != row.Profile || o.ExactSource.GaiaID != row.GaiaID || o.Proxy != cfg.Proxy || len(o.AccountIDs) != 0 {
					t.Fatal("rebind did not use exact source or original egress")
				}
				if kind == "exchange-error" {
					return nil, errors.New("fixture exchange failed")
				}
				resultRow := row
				if kind == "wrong-result-root" {
					resultRow.ChromeRoot = filepath.Join(row.ChromeRoot, "other")
				}
				return []chromeauth.ImportResult{fixtureChromeResult(t, resultRow)}, nil
			}
			prepared := 0
			var committed []bool
			admin := &runtimeAdmin{pool: pool, requests: newRequestRegistry(t.Context()), chrome: p, chromePrepareRebind: func(id string) (func(bool), error) {
				prepared++
				if id != account.ID {
					t.Fatal("reset another account")
				}
				if kind == "prepare-error" {
					return nil, errors.New("fixture prepare failed")
				}
				if kind == "write-error" {
					if err := os.Chmod(storage, 0o400); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(storage, 0o600) })
				}
				return func(ok bool) {
					committed = append(committed, ok)
					if kind == "write-error" {
						_ = os.Chmod(storage, 0o600)
					}
				}, nil
			}}
			input := api.ChromeImportInput{ChromeRoot: row.ChromeRoot, AccountIDs: []string{row.ID}, RebindAccountID: account.ID, ConfirmRebind: true}
			if kind == "unconfirmed" {
				input.ConfirmRebind = false
			}
			if kind == "wrong-target" {
				input.RebindAccountID = "other@example.com"
			}
			if kind == "mixed-environment" {
				input.Locale = "different"
			}
			if kind == "cancelled" {
				cancel()
			}
			if kind == "busy" {
				busy, e := pool.AcquireAccount(t.Context(), account.ID)
				if e != nil {
					t.Fatal(e)
				}
				defer busy.Release()
			}
			updated, err := admin.ImportChromeAccounts(ctx, input)
			success := strings.HasSuffix(kind, "success") || kind == "first-binding"
			if success {
				if err != nil || len(updated) != 1 || updated[0].ID != account.ID || p.imports != 1 || prepared != 1 || !reflect.DeepEqual(committed, []bool{true}) {
					t.Fatal("single-account source rebind failed")
				}
				stored, e := aistudio.LoadStorageState(storage)
				if e != nil {
					t.Fatal(e)
				}
				kept, _, _ := stored.AuthExtension()
				if kept.Source.ChromeRoot != row.ChromeRoot || kept.Source.Profile != row.Profile || kept.Source.GaiaID != row.GaiaID {
					t.Fatal("new source not persisted")
				}
			} else {
				if err == nil {
					t.Fatal("failed rebind reported success")
				}
				assertFixtureStateUnchanged(t, storage, before)
				if kind == "write-error" && !reflect.DeepEqual(committed, []bool{false}) {
					t.Fatal("failed write did not roll back prepared headers")
				}
				if kind != "wrong-result-root" && kind != "exchange-error" && kind != "prepare-error" && kind != "write-error" && p.imports != 0 {
					t.Fatal("rejected rebind fetched authentication")
				}
			}
			checks := map[string]bool{
				"configuration": account.Config == cfg, "identity": account.ID == cfg.Label,
				"models": reflect.DeepEqual(account.Models, beforeModels), "tier": account.BenefitTier == aistudio.BenefitTierPro,
				"quota": reflect.DeepEqual(pool.Status()[0].Cooldowns, beforeCooldowns),
			}
			for name, preserved := range checks {
				if !preserved {
					t.Errorf("rebind changed %s", name)
				}
			}
		})
	}
}
