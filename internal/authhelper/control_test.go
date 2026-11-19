package authhelper

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	for _, tc := range []struct {
		name          string
		dropBootstrap bool
	}{
		{name: "completed-with-old-ack"},
		{name: "interrupted-with-old-pending", dropBootstrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b, m, count := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			history := []aistudio.SessionOperation{
				{ID: "retired_fixture_001", Reason: "reauth", Epoch: 1, BaseMaterial: 1, StateRevision: 1, Deadline: 1},
				{ID: "retired_fixture_002", Reason: "reauth", Epoch: 1, BaseMaterial: 1, StateRevision: 2, Deadline: 2},
			}
			remote := aistudio.RemoteSession{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1, RetiredOperations: history}
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
			load := func() (aistudio.StorageState, *aistudio.RemoteSession) {
				t.Helper()
				s, err := aistudio.LoadStorageState(account.StoragePath)
				if err != nil {
					t.Fatal(err)
				}
				meta, err := s.Session()
				if err != nil || meta == nil || meta.Remote == nil {
					t.Fatal("missing remote session", err)
				}
				return s, meta.Remote
			}
			state, _ = load()
			if err := c.PrepareRecovery(ctx, &state, account.StoragePath, "bootstrap", nil); err != nil {
				t.Fatal(err)
			}
			if tc.dropBootstrap {
				meta, err := state.Session()
				if err != nil || meta == nil || meta.Remote == nil {
					t.Fatal("missing old remote session", err)
				}
				meta.Remote.Ack = nil
				meta.Remote.RecoveryRequired = true
				meta.Remote.Pending = &aistudio.SessionOperation{ID: "pending_fixture_001", Reason: "reauth", Epoch: 1, BaseCookie: meta.Remote.CookieRevision, BaseMaterial: 1, StateRevision: meta.Revision + 1, Deadline: 3}
				if err := state.SetSession(meta); err != nil {
					t.Fatal(err)
				}
			}
			if err := aistudio.WriteManagedSessionState(ctx, account.StoragePath, &state); err != nil {
				t.Fatal(err)
			}
			_, before := load()
			if !reflect.DeepEqual(before.RetiredOperations, history) {
				t.Fatal("fixture did not persist retired operations")
			}
			if err := SetAuthorityPolicy(ctx, m, 1, false, false); err != nil {
				t.Fatal(err)
			}
			if err := SetAuthorityPolicy(ctx, m, 2, true, false); err != nil {
				t.Fatal(err)
			}
			next := remote
			next.Epoch, next.CookieRevision = 3, 1
			next.RetiredOperations = nil // Native CLI supplies a new binding, not its history.
			if err := ReenrollNative(ctx, c, account.StoragePath, next, false); safeCode(err) != "ACK_REQUIRED" {
				t.Fatal("unacknowledged epoch silently retired", err)
			}
			if tc.dropBootstrap {
				c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/v1/refresh" {
						t.Errorf("unexpected RPC after failed bootstrap: %s", r.URL.Path)
					}
					return nil, errors.New("offline fixture: helper unavailable")
				})
			}
			err = ReenrollNative(ctx, c, account.StoragePath, next, true)
			if (!tc.dropBootstrap && err != nil) || (tc.dropBootstrap && safeCode(err) != "HELPER_UNAVAILABLE") {
				t.Fatal("unexpected reenrollment result", err)
			}
			_, got := load()
			if !reflect.DeepEqual(got.RetiredOperations, history) {
				t.Fatalf("retired operations lost or changed: got %d, want %d", len(got.RetiredOperations), len(history))
			}
			if len(got.Retired) != 1 || got.Retired[0].Epoch != before.Epoch || !reflect.DeepEqual(got.Retired[0].Pending, before.Pending) || !reflect.DeepEqual(got.Retired[0].Ack, before.Ack) || got.Epoch != 3 || count.Load() != 0 {
				t.Fatal("reenrollment contract")
			}
			if tc.dropBootstrap {
				if !got.EnrollmentRequired || got.Pending == nil || got.Pending.Reason != "bootstrap" || got.Pending.Epoch != 3 || got.CookieRevision != 1 || got.Ack != nil {
					t.Fatal("interrupted bootstrap lost enrollment fence")
				}
			} else if got.EnrollmentRequired || got.Pending != nil || got.Ack != nil || got.CookieRevision != 2 {
				t.Fatal("new enrollment did not complete")
			}
		})
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
