package authhelper

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

func TestNativeRenewReadsSourceOnlyOnExplicitMaterialRejection(t *testing.T) {
	for _, mode := range []string{"saved-success", "network-error", "rejected-no-permission", "rejected-exact"} {
		t.Run(mode, func(t *testing.T) {
			_, m, _ := fixtureBroker(t, nil)
			state, _ := aistudio.LoadStorageState(m.StateFile)
			ext, _, _ := state.AuthExtension()
			ext.Source.ChromeRoot = filepath.Join(t.TempDir(), "unmounted-dedicated-root")
			if err := state.SetAuthExtension(ext); err != nil {
				t.Fatal(err)
			}
			refreshes, imports := 0, 0
			refresh := func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
				refreshes++
				switch mode {
				case "network-error":
					return nil, errors.New("fixture network failure")
				case "rejected-no-permission", "rejected-exact":
					return nil, chromeauth.ErrCredentialsRejected
				}
				return fixtureCookies("saved-material-fixture"), nil
			}
			importer := func(ctx context.Context, o chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
				imports++
				if o.ExactSource == nil || o.ChromeRoot != ext.Source.ChromeRoot || !chromeauth.SameSource(*o.ExactSource, ext.Source) {
					t.Fatal("recovery changed source")
				}
				result := state
				result.Cookies = fixtureCookies("exact-recovery-fixture")
				return []chromeauth.ImportResult{{State: result}}, nil
			}
			_, err := nativeRenewWith(context.Background(), state, m, mode == "rejected-exact", refresh, importer)
			if refreshes != 1 {
				t.Fatal("saved material repeated")
			}
			if mode == "rejected-exact" {
				if err != nil || imports != 1 {
					t.Fatal("exact source recovery", err)
				}
			} else if imports != 0 {
				t.Fatal("source read on wrong failure")
			}
			if mode == "network-error" && safeCode(err) != "INDETERMINATE" {
				t.Fatal(err)
			}
			if mode == "rejected-no-permission" && safeCode(err) != "MATERIAL_REJECTED" {
				t.Fatal(err)
			}
		})
	}
}

func TestHealthAndTLSRejectionsDoNotReadAuthority(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	// Removing the fixture state proves health does not touch material storage.
	original := b.mappings[m.Handle]
	missing := original
	missing.StateFile = filepath.Join(t.TempDir(), "missing-state.json")
	b.mappings[m.Handle] = missing
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 0 {
		t.Fatal("health authenticated")
	}
}
