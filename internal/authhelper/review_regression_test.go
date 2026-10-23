package authhelper

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestHistoricalFailureIDCannotAuthenticateAgain(t *testing.T) {
	b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
		return s, failure("MATERIAL_REJECTED")
	})
	a := fixtureRequest(m, "history_operation_0001", 0)
	other := fixtureRequest(m, "history_operation_0002", 0)
	for _, r := range []RefreshRequest{a, other, a} {
		if _, err := b.Refresh(t.Context(), m.Consumer, r); safeCode(err) != "MATERIAL_REJECTED" {
			t.Fatal(err)
		}
	}
	if count.Load() != 2 {
		t.Fatal("history repeated OAuth")
	}
	a.BaseCookie++
	if _, err := b.Refresh(t.Context(), m.Consumer, a); safeCode(err) != "OPERATION_CONFLICT" {
		t.Fatal("historical ID accepted new parameters", err)
	}
}

func TestUndeliveredExpiredPendingHasDurableNoSendReceipt(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	state, path := fixtureRemoteState(t, m)
	original := c.http.Transport
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, failure("HELPER_UNAVAILABLE") })
	budget := func(string) error { return nil }
	if c.PrepareRecovery(t.Context(), &state, path, "reauth", budget) == nil {
		t.Fatal("lost pre-send accepted")
	}
	c.http.Transport = original
	future := time.Now().Add(5 * time.Minute)
	b.now = func() time.Time { return future }
	if err := c.PrepareRecovery(t.Context(), &state, path, "reauth", budget); safeCode(err) != "NOT_STARTED" {
		t.Fatal(err)
	}
	if count.Load() != 0 {
		t.Fatal("expired undelivered request authenticated")
	}
	actual, _ := aistudio.LoadStorageState(path)
	meta, _ := actual.Session()
	if meta.Remote.Pending != nil || !meta.Remote.RecoveryRequired || len(meta.Remote.RetiredOperations) != 1 {
		t.Fatal("proof not committed")
	}
	b.now = time.Now
	if err := c.PrepareRecovery(t.Context(), &actual, path, "reauth", budget); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatal("new logical recovery count")
	}
}

func TestPendingNoOpWriteAndExplicitKeepCurrentDisposition(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	state, path := fixtureRemoteState(t, m)
	original := c.http.Transport
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if response != nil {
			response.Body.Close()
		}
		if err != nil {
			return nil, err
		}
		return nil, failure("HELPER_UNAVAILABLE")
	})
	if c.PrepareRecovery(t.Context(), &state, path, "reauth", func(string) error { return nil }) == nil {
		t.Fatal("lost response accepted")
	}
	c.http.Transport = original
	state, _ = aistudio.LoadStorageState(path)
	meta, _ := state.Session()
	revision := meta.Revision
	if err := aistudio.WriteStorageState(path, state); err != nil {
		t.Fatal(err)
	}
	state, _ = aistudio.LoadStorageState(path)
	meta, _ = state.Session()
	if meta.Revision != revision {
		t.Fatal("no-op write changed revision")
	}
	state.Cookies = fixtureCookies("newer-local-fixture")
	if err := aistudio.WriteStorageState(path, state); err != nil {
		t.Fatal(err)
	}
	state, _ = aistudio.LoadStorageState(path)
	preserved := append([]aistudio.StateCookie(nil), state.Cookies...)
	if err := c.PrepareRecovery(t.Context(), &state, path, "reauth", func(string) error { return nil }); safeCode(err) != "VERSION_CONFLICT" {
		t.Fatal("normal retry must not overwrite newer local state", err)
	}
	if err := c.ReconcileKeepingCurrent(t.Context(), &state, path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Cookies, preserved) {
		t.Fatal("native disposition replaced current cookies")
	}
	if err := commitNativeProjection(t.Context(), c, &state, path); err != nil {
		t.Fatal(err)
	}
	meta, _ = state.Session()
	authority, _ := aistudio.LoadStorageState(m.StateFile)
	a, _ := authority.Session()
	if meta.Remote.Pending != nil || meta.Remote.Ack != nil || a.Authority.Operation.AckMode != "keep_current" || count.Load() != 1 {
		t.Fatal("control disposition incomplete")
	}
}

func TestReconcileResumesPublishedEnrollmentBeforePending(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	state, path := fixtureRemoteState(t, m)
	meta, _ := state.Session()
	meta.Remote.EnrollmentRequired = true
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), path, &state) != nil {
		t.Fatal("fixture save")
	}
	if err := reconcileProjection(t.Context(), c, path); err != nil {
		t.Fatal(err)
	}
	state, _ = aistudio.LoadStorageState(path)
	meta, _ = state.Session()
	if meta.Remote.EnrollmentRequired || meta.Remote.Pending != nil || len(state.Cookies) == 0 || count.Load() != 0 {
		t.Fatal("bootstrap not completed or authenticated unnecessarily")
	}
}

func TestReturnLocalPreservesMaterialAndAdvancesFutureEpoch(t *testing.T) {
	_, m, _ := fixtureBroker(t, nil)
	if err := SetAuthorityPolicy(t.Context(), m, 1, false, false); err != nil {
		t.Fatal(err)
	}
	original, _ := aistudio.LoadStorageState(m.StateFile)
	extension, _, _ := original.AuthExtension()
	if err := returnLocalAuthority(t.Context(), m, 2, true, ""); err != nil {
		t.Fatal(err)
	}
	local, _ := aistudio.LoadStorageState(m.StateFile)
	meta, _ := local.Session()
	archived, err := local.RetiredAuthority()
	nextExtension, _, _ := local.AuthExtension()
	if err != nil || meta != nil || archived == nil || archived.Epoch <= 2 || !reflect.DeepEqual(local.Cookies, original.Cookies) || !reflect.DeepEqual(extension, nextExtension) {
		t.Fatal("local return lost state")
	}
	if err := aistudio.WriteStorageState(m.StateFile, local); err != nil {
		t.Fatal("local cookie writer still fenced", err)
	}
	if err := registerAuthority(t.Context(), m, ""); err != nil {
		t.Fatal(err)
	}
	next, _ := aistudio.LoadStorageState(m.StateFile)
	meta, _ = next.Session()
	if meta.Authority.Epoch <= archived.Epoch {
		t.Fatal("old consumer epoch resurrected")
	}
}
