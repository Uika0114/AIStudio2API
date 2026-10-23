package authhelper

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

func TestCLIPlanNeverReadsConfigOrStartsProduct(t *testing.T) {
	for _, op := range []string{"serve", "register", "enable", "disable", "bootstrap", "reconcile", "rebind-source", "reenroll", "inspect"} {
		var out, errOut bytes.Buffer
		path := filepath.Join(t.TempDir(), "does-not-exist.json")
		if code := RunCLI([]string{"-operation", op, "-config", path}, &out, &errOut); code != 0 {
			t.Fatalf("plan failed for %s", op)
		}
		if !bytes.Contains(out.Bytes(), []byte(`"auth_read":false`)) || !bytes.Contains(out.Bytes(), []byte(`"config_read":false`)) {
			t.Fatal("plan read state")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("plan wrote config")
		}
	}
}

func TestRemoteProjectionStartsDisabledAndIncomplete(t *testing.T) {
	root := t.TempDir()
	state := aistudio.StorageState{}
	r := &aistudio.RemoteSession{Handle: "fixture_handle_0001", Consumer: "consumer.fixture.invalid", GaiaID: "fixture_gaia", Email: "fixture@example.invalid", Epoch: 1, MaterialVersion: 1}
	if err := state.SetSession(&aistudio.SessionState{Version: 1, Remote: r}); err != nil {
		t.Fatal(err)
	}
	account, lease, err := aistudio.NewAccountStore(root).CreateRemoteProjection(aistudio.DefaultAccountConfig(r.Email), state)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	actual, err := aistudio.LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := actual.Session()
	if account.Config.Enabled || !meta.Remote.EnrollmentRequired || len(actual.Cookies) != 0 {
		t.Fatal("projection started a generator")
	}
}

func TestReenrollFencesLateEpochWithoutDeletingPriorMetadata(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	remote := aistudio.RemoteSession{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1}
	state := aistudio.StorageState{}
	if err := state.SetSession(&aistudio.SessionState{Version: 1, Remote: &remote}); err != nil {
		t.Fatal(err)
	}
	account, lease, err := aistudio.NewAccountStore(t.TempDir()).CreateRemoteProjection(aistudio.DefaultAccountConfig(m.Email), state)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	state, _ = aistudio.LoadStorageState(account.StoragePath)
	if err := c.PrepareRecovery(context.Background(), &state, account.StoragePath, "bootstrap", nil); err != nil {
		t.Fatal(err)
	}
	// Deliberately keep the old ack pending before fencing its epoch.
	if err := aistudio.WriteManagedSessionState(context.Background(), account.StoragePath, &state); err != nil {
		t.Fatal(err)
	}
	if err := SetAuthorityPolicy(context.Background(), m, 1, false, false); err != nil {
		t.Fatal(err)
	}
	if err := SetAuthorityPolicy(context.Background(), m, 2, true, false); err != nil {
		t.Fatal(err)
	}
	next := remote
	next.Epoch = 3
	next.CookieRevision = 1
	if err := ReenrollNative(context.Background(), c, account.StoragePath, next, false); safeCode(err) != "ACK_REQUIRED" {
		t.Fatal("unacknowledged epoch silently retired", err)
	}
	if err := ReenrollNative(context.Background(), c, account.StoragePath, next, true); err != nil {
		t.Fatal(err)
	}
	state, _ = aistudio.LoadStorageState(account.StoragePath)
	meta, _ := state.Session()
	if len(meta.Remote.Retired) != 1 || meta.Remote.Retired[0].Ack == nil || meta.Remote.Epoch != 3 || meta.Remote.EnrollmentRequired || meta.Remote.CookieRevision != 2 || count.Load() != 0 {
		t.Fatal("reenrollment contract")
	}
}

func TestNativeExactRebindFailureKeepsOriginalMaterial(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "success"}[succeed], func(t *testing.T) {
			b, m, _ := fixtureBroker(t, nil)
			_ = b
			if err := SetAuthorityPolicy(context.Background(), m, 1, false, false); err != nil {
				t.Fatal(err)
			}
			state, _ := aistudio.LoadStorageState(m.StateFile)
			old, _, _ := state.AuthExtension()
			// The fixture authority deliberately has no SID; no real identity/CNG is used.
			root := filepath.Join(t.TempDir(), "dedicated-chrome")
			if err := os.MkdirAll(filepath.Join(root, "Profile 2"), 0700); err != nil {
				t.Fatal(err)
			}
			source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: root, Profile: "Profile 2", GaiaID: m.GaiaID, Email: m.Email}
			calls := 0
			importer := func(ctx context.Context, o chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
				calls++
				if o.ExactSource == nil || o.ChromeRoot != root || o.ExactSource.Profile != "Profile 2" || o.ExactSource.GaiaID != m.GaiaID {
					t.Fatal("not exact")
				}
				if !succeed {
					return nil, errors.New("fixture unobservable upstream outcome")
				}
				result := aistudio.StorageState{Cookies: fixtureCookies("rebind-fixture")}
				ext := aistudio.AuthExtension{Source: source, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: m.GaiaID, RefreshToken: "new-fixture-token", WrappedBindingKey: []byte("new-fixture-key")}}
				if err := result.SetAuthExtension(ext); err != nil {
					t.Fatal(err)
				}
				return []chromeauth.ImportResult{{State: result}}, nil
			}
			err := rebindAuthoritySource(context.Background(), m, 2, source, false, "", importer)
			actual, _ := aistudio.LoadStorageState(m.StateFile)
			ext, _, _ := actual.AuthExtension()
			meta, _ := actual.Session()
			if calls != 1 {
				t.Fatal("unexpected import count")
			}
			if succeed {
				if err != nil || ext.Source.Profile != "Profile 2" || meta.Authority.Epoch != 3 || meta.Authority.MaterialVersion != 2 || meta.Authority.Enabled {
					t.Fatal("rebind success", err)
				}
			} else {
				if safeCode(err) != "INDETERMINATE" || ext.OAuth.RefreshToken != old.OAuth.RefreshToken || ext.Source != old.Source || meta.Authority.Operation.Stage != "indeterminate" {
					t.Fatal("failed rebind replaced original material", err)
				}
			}
		})
	}
}
