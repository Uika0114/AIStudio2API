package authhelper

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func fixtureCookies(value string) []aistudio.StateCookie {
	result := make([]aistudio.StateCookie, 0, 3)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		result = append(result, aistudio.StateCookie{Name: name, Value: value, Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None"})
	}
	return result
}
func fixtureBroker(t *testing.T, renew RenewFunc) (*Broker, Mapping, *atomic.Int32) {
	t.Helper()
	mapping := Mapping{Handle: "fixture_handle_0001", Consumer: "consumer.fixture.invalid", GaiaID: "fixture_gaia", Email: "fixture@example.invalid", StateFile: filepath.Join(t.TempDir(), "fixture", "storage-state.json"), DirectAuth: true}
	state := aistudio.StorageState{Cookies: fixtureCookies("initial-fixture")}
	ext := aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", ChromeRoot: filepath.Join(t.TempDir(), "fixture-only"), Profile: "Default", GaiaID: mapping.GaiaID, Email: mapping.Email}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: mapping.GaiaID, RefreshToken: "fixture-not-a-token", WrappedBindingKey: []byte("fixture-not-a-key")}}
	if err := state.SetAuthExtension(ext); err != nil {
		t.Fatal(err)
	}
	meta := &aistudio.SessionState{Version: Protocol, Authority: &aistudio.SessionAuthority{Handle: mapping.Handle, Consumer: mapping.Consumer, GaiaID: mapping.GaiaID, Email: mapping.Email, Epoch: 1, MaterialVersion: 1, Enabled: true}}
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteManagedSessionState(context.Background(), mapping.StateFile, &state); err != nil {
		t.Fatal(err)
	}
	count := &atomic.Int32{}
	if renew == nil {
		renew = func(ctx context.Context, s aistudio.StorageState, m Mapping, allow bool) (aistudio.StorageState, error) {
			s.Cookies = fixtureCookies("renewed-fixture")
			return s, nil
		}
	}
	b, err := NewBroker(context.Background(), []Mapping{mapping}, func(ctx context.Context, s aistudio.StorageState, m Mapping, allow bool) (aistudio.StorageState, error) {
		count.Add(1)
		return renew(ctx, s, m, allow)
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, mapping, count
}
func fixtureRequest(m Mapping, id string, base uint64) RefreshRequest {
	return RefreshRequest{Version: Protocol, Handle: m.Handle, Epoch: 1, OperationID: id, BaseCookie: base, BaseMaterial: 1, Reason: "reauth", Deadline: time.Now().Add(time.Minute).Unix()}
}
func ackResult(t *testing.T, b *Broker, m Mapping, r *RefreshResponse) {
	t.Helper()
	a := AckRequest{Version: Protocol, Handle: m.Handle, Epoch: r.Epoch, OperationID: r.OperationID, CookieRevision: r.CookieRevision, MaterialVersion: r.MaterialVersion}
	if err := b.Ack(context.Background(), m.Consumer, a); err != nil {
		t.Fatal(err)
	}
	if err := b.Ack(context.Background(), m.Consumer, a); err != nil {
		t.Fatal("ack is not idempotent", err)
	}
}
func TestBrokerDurableResultReplayAndAck(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	req := fixtureRequest(m, "operation_fixture_0001", 0)
	result, err := b.Refresh(context.Background(), m.Consumer, req)
	if err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	again, err := b.Refresh(context.Background(), m.Consumer, req)
	if err != nil || again.CookieRevision != result.CookieRevision || count.Load() != 1 {
		t.Fatal("expired request did not replay saved result", err)
	}
	changed := req
	changed.Reason = "bootstrap"
	if _, err := b.Refresh(context.Background(), m.Consumer, changed); safeCode(err) != "OPERATION_CONFLICT" {
		t.Fatal("ID parameter collision accepted")
	}
	another := fixtureRequest(m, "operation_fixture_0002", 1)
	if _, err := b.Refresh(context.Background(), m.Consumer, another); safeCode(err) != "ACK_REQUIRED" {
		t.Fatal("unacked result overwritten")
	}
	ackResult(t, b, m, result)
	b.now = time.Now
	if _, err := b.Refresh(context.Background(), m.Consumer, another); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("unexpected authentication count")
	}
}
func TestBrokerBootstrapHasNoGoogleWork(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	req := fixtureRequest(m, "operation_bootstrap_1", 0)
	req.Reason = "bootstrap"
	r, err := b.Refresh(context.Background(), m.Consumer, req)
	if err != nil || count.Load() != 0 || r.CookieRevision != 1 {
		t.Fatal("bootstrap authenticated", err)
	}
}
func TestBrokerUnknownAuthenticationAndFailedSaveDoNotRetry(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "result-save"}[failSave], func(t *testing.T) {
			renew := RenewFunc(func(ctx context.Context, s aistudio.StorageState, m Mapping, allow bool) (aistudio.StorageState, error) {
				return s, errors.New("fixture secret must not be exposed")
			})
			if failSave {
				renew = nil
			}
			b, m, count := fixtureBroker(t, renew)
			if failSave {
				writes := 0
				b.write = func(ctx context.Context, p string, s *aistudio.StorageState) error {
					writes++
					if writes == 2 {
						return errors.New("fixture disk failure")
					}
					return aistudio.WriteManagedSessionState(ctx, p, s)
				}
			}
			req := fixtureRequest(m, "operation_unknown_01", 0)
			if _, err := b.Refresh(context.Background(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
				t.Fatal(err)
			}
			if _, err := b.Refresh(context.Background(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
				t.Fatal(err)
			}
			if count.Load() != 1 {
				t.Fatal("unknown operation was repeated")
			}
			state, err := aistudio.LoadStorageState(m.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			if state.Cookies[0].Value != "initial-fixture" {
				t.Fatal("failed result replaced cookies")
			}
		})
	}
}
func TestBrokerRefusesAliasesWrongConsumerAndLateEpoch(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	req := fixtureRequest(m, "operation_identity_1", 0)
	if _, err := b.Refresh(context.Background(), "wrong.fixture.invalid", req); safeCode(err) != "IDENTITY_MISMATCH" {
		t.Fatal(err)
	}
	req.Epoch = 2
	if _, err := b.Refresh(context.Background(), m.Consumer, req); safeCode(err) != "IDENTITY_MISMATCH" {
		t.Fatal(err)
	}
	alias := m
	alias.Handle = "fixture_handle_0002"
	if _, err := NewBroker(context.Background(), []Mapping{m, alias}, b.renew); err == nil {
		t.Fatal("same account alias accepted")
	}
	if count.Load() != 0 {
		t.Fatal("identity failure authenticated")
	}
}
func TestBrokerRestartedPendingCannotAuthenticateAgain(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	req := fixtureRequest(m, "operation_restart_01", 0)
	state, _ := aistudio.LoadStorageState(m.StateFile)
	meta, _ := state.Session()
	meta.Authority.Operation = &aistudio.SessionOperation{ID: req.OperationID, Epoch: 1, BaseMaterial: 1, Reason: req.Reason, Deadline: req.Deadline, Stage: "pending"}
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteManagedSessionState(context.Background(), m.StateFile, &state); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Refresh(context.Background(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
		t.Fatal(err)
	}
	if count.Load() != 0 {
		t.Fatal("restart repeated authentication")
	}
}
func TestCookiePolicyRejectsDomainPartitionAndAmbiguity(t *testing.T) {
	if err := ValidateCookies(fixtureCookies("fixture"), nil); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]aistudio.StateCookie) []aistudio.StateCookie{
		func(c []aistudio.StateCookie) []aistudio.StateCookie {
			c[0].Domain = ".google.com.evil.invalid"
			return c
		},
		func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].PartitionKey = "fixture"; return c },
		func(c []aistudio.StateCookie) []aistudio.StateCookie { return append(c, c[0]) },
	} {
		if err := ValidateCookies(mutate(fixtureCookies("fixture")), nil); err == nil {
			t.Fatal("unsafe cookie accepted")
		}
	}
}
