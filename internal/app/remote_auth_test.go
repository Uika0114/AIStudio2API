package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type remoteProviderFixture struct {
	prepare func(context.Context, *aistudio.StorageState, string, string, func(string) error) error
	ack     func(context.Context, *aistudio.RemoteSession) error
}

func (f *remoteProviderFixture) Consumer() string { return "consumer.fixture.invalid" }
func (f *remoteProviderFixture) PrepareRecovery(c context.Context, s *aistudio.StorageState, p, r string, b func(string) error) error {
	return f.prepare(c, s, p, r, b)
}
func (f *remoteProviderFixture) AckSession(c context.Context, r *aistudio.RemoteSession) error {
	if f.ack != nil {
		return f.ack(c, r)
	}
	return nil
}

func fixtureRemoteRefresher(t *testing.T) (*authRuntimeRefresher, context.Context, string, *remoteProviderFixture, *int) {
	t.Helper()
	r, ctx, path := fixtureAuthRefresher(t, nil)
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	meta := &aistudio.SessionState{Version: 1, Remote: &aistudio.RemoteSession{Handle: "fixture_handle_0001", Consumer: "consumer.fixture.invalid", GaiaID: "fixture_gaia", Email: "fixture@example.com", Epoch: 1, MaterialVersion: 1}}
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteManagedSessionState(ctx, path, &state); err != nil {
		t.Fatal(err)
	}
	count, sequence := new(int), 0
	fixture := &remoteProviderFixture{}
	fixture.prepare = func(c context.Context, s *aistudio.StorageState, p, reason string, take func(string) error) error {
		sequence++
		id := fmt.Sprintf("fixture_operation_%04d", sequence)
		if err := take(id); err != nil {
			return err
		}
		*count++
		m, _ := s.Session()
		remote := m.Remote
		remote.Pending = &aistudio.SessionOperation{ID: id, Reason: reason, Epoch: remote.Epoch, BaseCookie: remote.CookieRevision, BaseMaterial: remote.MaterialVersion, StateRevision: m.Revision + 1, Deadline: time.Now().Add(time.Minute).Unix()}
		if err := s.SetSession(m); err != nil {
			return err
		}
		if err := aistudio.WriteManagedSessionState(c, p, s); err != nil {
			return err
		}
		m, _ = s.Session()
		remote = m.Remote
		s.Cookies = fixtureRefreshCookies("fixture-remote-new")
		remote.Pending = nil
		remote.CookieRevision++
		remote.Ack = &aistudio.SessionAck{ID: id, Epoch: remote.Epoch, Cookie: remote.CookieRevision, Material: remote.MaterialVersion}
		return s.SetSession(m)
	}
	r.remote = fixture
	r.refresh = func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
		t.Fatal("remote called local CNG")
		return nil, nil
	}
	r.importCurrent = func(context.Context, aistudio.AuthSource, *aistudio.ChromeOAuthMaterial, string) (aistudio.StorageState, error) {
		t.Fatal("remote scanned Chrome")
		return aistudio.StorageState{}, nil
	}
	return r, aistudio.WithRemoteAuthBudget(ctx), path, fixture, count
}
func TestRemoteRefresherCommitsThenAcksPreservingCooldown(t *testing.T) {
	r, ctx, path, fixture, count := fixtureRemoteRefresher(t)
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	account := lease.Account()
	original := account.Config
	resets, acks := 0, 0
	r.reset = func(string) error {
		resets++
		disk, _ := aistudio.LoadStorageState(path)
		if disk.Cookies[0].Value != "fixture-old" {
			t.Fatal("cookies committed before runtime preparation")
		}
		return nil
	}
	fixture.ack = func(context.Context, *aistudio.RemoteSession) error {
		acks++
		disk, _ := aistudio.LoadStorageState(path)
		if disk.Cookies[0].Value != "fixture-remote-new" {
			t.Fatal("ack before commit")
		}
		return nil
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	disk, _ := aistudio.LoadStorageState(path)
	meta, _ := disk.Session()
	if resets != 1 || acks != 1 || *count != 1 || meta.Remote.Ack != nil || meta.Remote.Pending != nil || account.Config != original {
		t.Fatal("commit/ack contract")
	}
	if ext, exists, _ := disk.AuthExtension(); exists && (ext.OAuth != nil || ext.Source.ChromeRoot != "") {
		t.Fatal("Windows materials copied")
	}
	if err := r.Refresh(ctx); !aistudio.IsSessionFailureCode(err, "RECOVERY_BUDGET_EXHAUSTED") {
		t.Fatal("logical budget reset", err)
	}
	if *count != 1 {
		t.Fatal("second authentication workflow")
	}
}
func TestRemoteFailureCannotEscapeThroughJoinedNetworkOr401(t *testing.T) {
	r, ctx, path, fixture, _ := fixtureRemoteRefresher(t)
	reset := 0
	r.reset = func(string) error { reset++; return nil }
	cause := &aistudio.RPCError{StatusCode: http.StatusUnauthorized, Code: 16, Message: "fixture authentication rejected"}
	fixture.prepare = func(context.Context, *aistudio.StorageState, string, string, func(string) error) error {
		return errors.Join(cause, &url.Error{Op: "POST", URL: "https://127.0.0.1:1", Err: context.DeadlineExceeded})
	}
	err := r.Recover(ctx, cause)
	if !aistudio.IsSessionFailure(err) || retryableGenerateAccountError(ctx, err) || proxyNetworkError(ctx, err) || aistudio.DefinitiveAuthenticationFailure(err) {
		t.Fatal("remote failure reached generic recovery", err)
	}
	state, _ := aistudio.LoadStorageState(path)
	if reset != 0 || state.Cookies[0].Value != "fixture-old" {
		t.Fatal("helper failure reset or overwrote session")
	}
	if r.pool.Status()[0].State == aistudio.AccountAuthRequired {
		t.Fatal("helper outage became account login failure")
	}
	joined := errors.Join(err, cause, &url.Error{Err: context.DeadlineExceeded})
	if retryableGenerateAccountError(ctx, joined) || proxyNetworkError(ctx, joined) || aistudio.DefinitiveAuthenticationFailure(joined) {
		t.Fatal("joined marker lost priority")
	}
}
func TestRemoteCASConflictCheckedBeforeRuntimeReset(t *testing.T) {
	r, ctx, path, fixture, _ := fixtureRemoteRefresher(t)
	prepare := fixture.prepare
	fixture.prepare = func(c context.Context, s *aistudio.StorageState, p, reason string, take func(string) error) error {
		if err := prepare(c, s, p, reason, take); err != nil {
			return err
		}
		raced, err := aistudio.LoadStorageState(p)
		if err != nil {
			return err
		}
		raced.Cookies = fixtureRefreshCookies("fixture-raced-writer")
		return aistudio.WriteStorageState(p, raced)
	}
	resets := 0
	r.reset = func(string) error { resets++; return nil }
	if err := r.Refresh(ctx); !aistudio.IsSessionFailureCode(err, "VERSION_CONFLICT") {
		t.Fatal(err)
	}
	disk, _ := aistudio.LoadStorageState(path)
	if resets != 0 || disk.Cookies[0].Value != "fixture-raced-writer" {
		t.Fatal("stale result reset or overwrote current session")
	}
}
func TestRemoteAckLossDoesNotUndoCommittedCookies(t *testing.T) {
	r, ctx, path, fixture, _ := fixtureRemoteRefresher(t)
	fixture.ack = func(context.Context, *aistudio.RemoteSession) error {
		return aistudio.SessionError("HELPER_UNAVAILABLE")
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal("ack loss broke committed recovery", err)
	}
	disk, _ := aistudio.LoadStorageState(path)
	meta, _ := disk.Session()
	if disk.Cookies[0].Value != "fixture-remote-new" || meta.Remote.Ack == nil {
		t.Fatal("lost durable ack state")
	}
}
