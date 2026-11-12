package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const recoveryFixtureID1 = "recovery_fixture_id_0001"
const recoveryFixtureID2 = "recovery_fixture_id_0002"
const recoveryFixtureID3 = "recovery_fixture_id_0003"
const recoveryFixtureID4 = "recovery_fixture_id_0004"

type recoveryControlFixture struct {
	path string
	r    *RemoteSession
	now  time.Time
}

func newRecoveryControlFixture(t *testing.T) recoveryControlFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture", "storage-state.json")
	state := sessionTestState(t, false)
	if err := WriteManagedSessionState(t.Context(), path, &state); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireSessionAccountLease(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	meta, _ := state.Session()
	return recoveryControlFixture{path: path, r: meta.Remote, now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
}

func (f recoveryControlFixture) start(t *testing.T, id string, background bool, now time.Time) {
	t.Helper()
	if err := ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, id, background, now); err != nil {
		t.Fatal(err)
	}
	f.r.Pending = &SessionOperation{ID: id, Reason: "reauth", Epoch: f.r.Epoch, BaseCookie: f.r.CookieRevision, BaseMaterial: f.r.MaterialVersion, Deadline: now.Add(90 * time.Second).Unix()}
	f.r.RecoveryRequired = true
}

func (f recoveryControlFixture) retire(t *testing.T, now time.Time) {
	t.Helper()
	old := *f.r.Pending
	f.r.Pending = nil
	f.r.RetiredOperations = append(f.r.RetiredOperations, old)
	if err := RetireRemoteRecoveryNotStarted(t.Context(), f.path, f.r, old, now); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryControlFixture) status(t *testing.T, enabled bool, now time.Time) RemoteRecoveryStatus {
	t.Helper()
	s, err := ReadRemoteRecoveryStatus(f.path, f.r, enabled, now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func recoveryRequireCode(t *testing.T, err error, code string) {
	t.Helper()
	if !IsSessionFailureCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestRemoteRecoveryControlAuthLimitAndExactRetirement(t *testing.T) {
	f := newRecoveryControlFixture(t)
	f.start(t, recoveryFixtureID1, false, f.now)
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "INDETERMINATE", f.now); err != nil {
		t.Fatal(err)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, true, f.now.Add(time.Minute)), "RECOVERY_BUDGET_REQUIRED")
	original := *f.r.Pending
	f.r.Pending = nil
	recoveryRequireCode(t, RetireRemoteRecoveryNotStarted(t.Context(), f.path, f.r, original, f.now), "IDENTITY_MISMATCH")
	f.r.Pending = &original
	retiredAt := f.now.Add(10 * time.Second)
	f.retire(t, retiredAt)
	// Duplicate retirement cannot reset the next due time.
	if err := RetireRemoteRecoveryNotStarted(t.Context(), f.path, f.r, original, retiredAt.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	s := f.status(t, true, retiredAt)
	if s.Action != "new_auth" || s.AuthUsed != 1 || !s.Due.Equal(retiredAt.Add(time.Minute)) {
		t.Fatalf("first retirement: %+v", s)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, false, s.Due), "RECOVERY_BUDGET_REQUIRED")
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, true, s.Due.Add(-time.Nanosecond)), "RECOVERY_BACKOFF")
	f.start(t, recoveryFixtureID2, true, s.Due)
	secondRetired := s.Due.Add(10 * time.Second)
	f.retire(t, secondRetired)
	s = f.status(t, true, secondRetired)
	if s.AuthUsed != 2 || !s.Due.Equal(secondRetired.Add(5*time.Minute)) || !s.Deadline.Equal(f.now.Add(15*time.Minute)) {
		t.Fatalf("second retirement: %+v", s)
	}
	f.start(t, recoveryFixtureID3, true, s.Due)
	f.retire(t, s.Due.Add(time.Second))
	s = f.status(t, true, s.Due.Add(6*time.Minute))
	if s.Action != "none" || s.Code != "RECOVERY_BUDGET_EXHAUSTED" || s.AuthUsed != 3 {
		t.Fatalf("auth exhausted: %+v", s)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID4, true, f.now.Add(14*time.Minute)), "RECOVERY_BUDGET_EXHAUSTED")
	control, err := loadRemoteRecoveryControl(f.path)
	if err != nil || control.Round.FirstID != recoveryFixtureID1 || !control.Round.Started.Equal(f.now) {
		t.Fatalf("round origin changed: %+v %v", control, err)
	}
}

func TestRemoteRecoveryControlPendingIntervalsAndCounts(t *testing.T) {
	f := newRecoveryControlFixture(t)
	f.start(t, recoveryFixtureID1, false, f.now)
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "HELPER_UNAVAILABLE", f.now); err != nil {
		t.Fatal(err)
	}
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "INDETERMINATE", f.now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, f.now.Add(29*time.Second)), "RECOVERY_BACKOFF")
	next := f.now.Add(30 * time.Second)
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, next); err != nil {
			t.Fatal(err)
		}
		finished := next.Add(10 * time.Second)
		if err := FinishRemoteRecoveryPending(t.Context(), f.path, f.r, recoveryFixtureID1, attempt == 3, "HELPER_UNAVAILABLE", finished); err != nil {
			t.Fatal(err)
		}
		// Finishing the same reservation twice does not double count commits.
		if err := FinishRemoteRecoveryPending(t.Context(), f.path, f.r, recoveryFixtureID1, true, "HELPER_UNAVAILABLE", finished.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		s := f.status(t, true, finished)
		if s.PendingUsed != attempt || s.Queries != attempt || s.AuthUsed != 1 {
			t.Fatalf("attempt %d counters: %+v", attempt, s)
		}
		if attempt < 3 {
			delay := 2 * time.Minute
			if attempt == 2 {
				delay = 5 * time.Minute
			}
			if !s.Due.Equal(finished.Add(delay)) {
				t.Fatalf("completion-based backoff: %+v", s)
			}
			recoveryRequireCode(t, ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, s.Due.Add(-time.Nanosecond)), "RECOVERY_BACKOFF")
			next = s.Due
		} else if s.Action != "none" || s.Code != "RECOVERY_BUDGET_EXHAUSTED" || s.Commits != 1 {
			t.Fatalf("pending exhausted: %+v", s)
		}
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryPending(context.Background(), f.path, f.r, f.now.Add(14*time.Minute)), "RECOVERY_BUDGET_EXHAUSTED")
}

func TestRemoteRecoveryControlCrashKeepsReservationWithoutCatchup(t *testing.T) {
	f := newRecoveryControlFixture(t)
	f.start(t, recoveryFixtureID1, false, f.now)
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "INDETERMINATE", f.now); err != nil {
		t.Fatal(err)
	}
	if err := ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, f.now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	// No Finish call: a new caller after restart reloads the spent reservation.
	restarted := f.now.Add(5 * time.Minute)
	recoveryRequireCode(t, ReserveRemoteRecoveryPending(context.Background(), f.path, f.r, restarted), "RECOVERY_BACKOFF")
	s := f.status(t, true, restarted)
	if s.PendingUsed != 1 || !s.Due.Equal(restarted.Add(2*time.Minute)) {
		t.Fatalf("crash refunded or burst: %+v", s)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, restarted), "RECOVERY_BACKOFF")
	if err := ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, s.Due); err != nil {
		t.Fatal(err)
	}
	s = f.status(t, true, s.Due)
	if s.PendingUsed != 2 {
		t.Fatalf("second reserved slot missing: %+v", s)
	}
}

func TestRemoteRecoveryControlDisabledAndDeadlineDoNotReset(t *testing.T) {
	f := newRecoveryControlFixture(t)
	f.start(t, recoveryFixtureID1, false, f.now)
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "INDETERMINATE", f.now); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(remoteRecoveryPath(f.path))
	s := f.status(t, false, f.now.Add(time.Minute))
	if s.Action != "none" || s.Code != "ACCOUNT_DISABLED" || s.AuthUsed != 1 {
		t.Fatalf("disabled: %+v", s)
	}
	after, _ := os.ReadFile(remoteRecoveryPath(f.path))
	if !bytes.Equal(before, after) {
		t.Fatal("disabled status changed budget")
	}
	s = f.status(t, true, f.now.Add(15*time.Minute))
	if s.Action != "none" || s.Code != "RECOVERY_BUDGET_EXPIRED" || !s.Deadline.Equal(f.now.Add(15*time.Minute)) {
		t.Fatalf("reenable reset deadline: %+v", s)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, s.Deadline), "RECOVERY_BUDGET_EXPIRED")
}

func (f recoveryControlFixture) commit(t *testing.T, now time.Time) {
	t.Helper()
	id := f.r.Pending.ID
	f.r.Pending, f.r.RecoveryRequired = nil, false
	f.r.CookieRevision++
	f.r.Ack = &SessionAck{ID: id, Epoch: f.r.Epoch, Cookie: f.r.CookieRevision, Material: f.r.MaterialVersion}
	if err := CompleteRemoteRecoveryAuth(t.Context(), f.path, f.r, now); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteRecoveryControlAckHasIndependentBoundedBudget(t *testing.T) {
	f := newRecoveryControlFixture(t)
	f.start(t, recoveryFixtureID1, false, f.now)
	recoveryRequireCode(t, CompleteRemoteRecoveryAuth(t.Context(), f.path, f.r, f.now), "RECOVERY_BUDGET_REQUIRED")
	f.commit(t, f.now.Add(time.Minute))
	s := f.status(t, true, f.now.Add(time.Minute))
	if s.Action != "none" || s.AuthUsed != 0 || s.Code != "RECOVERY_BUDGET_REQUIRED" {
		t.Fatalf("initial ACK has no guessed failure time: %+v", s)
	}
	firstFailure := f.now.Add(2 * time.Minute)
	if err := RecordRemoteRecoveryAckFailure(t.Context(), f.path, f.r, "HELPER_UNAVAILABLE", firstFailure); err != nil {
		t.Fatal(err)
	}
	if err := RecordRemoteRecoveryAckFailure(t.Context(), f.path, f.r, "HELPER_UNAVAILABLE", firstFailure.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s = f.status(t, true, firstFailure)
	if s.Action != "ack" || !s.Deadline.Equal(firstFailure.Add(15*time.Minute)) || !s.Due.Equal(firstFailure.Add(30*time.Second)) {
		t.Fatalf("ACK origin reset: %+v", s)
	}
	next := s.Due
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ReserveRemoteRecoveryAck(t.Context(), f.path, f.r, next); err != nil {
			t.Fatal(err)
		}
		// Remote ACK success alone cannot remove a still-durable local ACK.
		recoveryRequireCode(t, CompleteRemoteRecoveryAck(t.Context(), f.path, f.r, recoveryFixtureID1), "ACK_REQUIRED")
		finished := next.Add(time.Second)
		if err := FinishRemoteRecoveryAck(t.Context(), f.path, f.r, recoveryFixtureID1, "STATE_UNAVAILABLE", finished); err != nil {
			t.Fatal(err)
		}
		s = f.status(t, true, finished)
		if s.AckUsed != attempt || s.AuthUsed != 0 {
			t.Fatalf("ack counters: %+v", s)
		}
		next = s.Due
	}
	if s.Action != "none" || s.Code != "RECOVERY_BUDGET_EXHAUSTED" {
		t.Fatalf("ACK did not stop: %+v", s)
	}
	if f.r.Pending != nil || f.r.RecoveryRequired || f.r.CookieRevision != 1 {
		t.Fatal("ACK failure changed committed session")
	}
	f.r.Ack = nil
	if err := CompleteRemoteRecoveryAck(t.Context(), f.path, f.r, recoveryFixtureID1); err != nil {
		t.Fatal(err)
	}
	s = f.status(t, true, next)
	if s.Action != "none" || s.AckUsed != 0 {
		t.Fatalf("cleared ACK retained scheduling: %+v", s)
	}
	// A new normal authentication is possible only after successful adoption.
	f.start(t, recoveryFixtureID2, false, next)
	s = f.status(t, true, next)
	if s.AuthUsed != 1 {
		t.Fatalf("new completed-cycle round: %+v", s)
	}
}

func TestRemoteRecoveryControlMissingCorruptAndOldBindingFailClosed(t *testing.T) {
	t.Run("legacy pending", func(t *testing.T) {
		f := newRecoveryControlFixture(t)
		f.r.Pending = &SessionOperation{ID: recoveryFixtureID1}
		f.r.RecoveryRequired = true
		recoveryRequireCode(t, RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "INDETERMINATE", f.now), "RECOVERY_BUDGET_REQUIRED")
		recoveryRequireCode(t, ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, f.now), "RECOVERY_BUDGET_REQUIRED")
		if _, err := os.Stat(remoteRecoveryPath(f.path)); !os.IsNotExist(err) {
			t.Fatal("old pending gained a budget")
		}
	})
	t.Run("legacy ACK", func(t *testing.T) {
		f := newRecoveryControlFixture(t)
		f.r.Ack = &SessionAck{ID: recoveryFixtureID1, Epoch: f.r.Epoch}
		recoveryRequireCode(t, RecordRemoteRecoveryAckFailure(t.Context(), f.path, f.r, "HELPER_UNAVAILABLE", f.now), "RECOVERY_BUDGET_REQUIRED")
		recoveryRequireCode(t, ReserveRemoteRecoveryAck(t.Context(), f.path, f.r, f.now), "RECOVERY_BUDGET_REQUIRED")
	})
	t.Run("corrupt", func(t *testing.T) {
		f := newRecoveryControlFixture(t)
		if err := os.WriteFile(remoteRecoveryPath(f.path), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID1, false, f.now), "RECOVERY_CONTROL_INVALID")
	})
	t.Run("binding", func(t *testing.T) {
		f := newRecoveryControlFixture(t)
		f.start(t, recoveryFixtureID1, false, f.now)
		f.r.Epoch++
		recoveryRequireCode(t, RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "INDETERMINATE", f.now), "RECOVERY_BINDING_MISMATCH")
		f.r.Pending, f.r.RecoveryRequired = nil, false
		recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, true, f.now), "RECOVERY_BINDING_MISMATCH")
		f.start(t, recoveryFixtureID2, false, f.now.Add(time.Minute))
		s := f.status(t, true, f.now.Add(time.Minute))
		if s.AuthUsed != 1 {
			t.Fatalf("explicit new binding not isolated: %+v", s)
		}
	})
}

func TestRemoteRecoveryControlPreoccupationFailureAndSafeContents(t *testing.T) {
	f := newRecoveryControlFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(ctx, f.path, f.r, recoveryFixtureID1, false, f.now), "CANCELED")
	if _, err := os.Stat(remoteRecoveryPath(f.path)); !os.IsNotExist(err) {
		t.Fatal("canceled reservation wrote a budget")
	}
	f.start(t, recoveryFixtureID1, false, f.now)
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "credential_sentinel_should_not_persist", f.now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(remoteRecoveryPath(f.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credential_sentinel", "SAPISID", "fixture@example.invalid", "cookies", "refresh_token", "deadline_secret"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("control contains forbidden data %q", forbidden)
		}
	}
	var c remoteRecoveryControl
	if json.Unmarshal(data, &c) != nil || c.Round.Pending.Stop != "" {
		t.Fatal("unknown error became persistent stop")
	}
	if err := RecordRemoteRecoveryFailure(t.Context(), f.path, f.r, "IDENTITY_MISMATCH", f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s := f.status(t, true, f.now.Add(time.Minute))
	if s.Action != "none" || s.Code != "IDENTITY_MISMATCH" {
		t.Fatalf("terminal error did not persist: %+v", s)
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryPending(t.Context(), f.path, f.r, f.now.Add(time.Minute)), "IDENTITY_MISMATCH")
}

func TestRemoteRecoveryControlSaveFailureDoesNotAuthorizeAction(t *testing.T) {
	f := newRecoveryControlFixture(t)
	// A non-directory storage parent prevents persistent control-file reservation.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(blocker, "storage-state.json")
	err := ReserveRemoteRecoveryAuth(t.Context(), badPath, f.r, recoveryFixtureID1, false, f.now)
	if err == nil {
		t.Fatal("failed persistent reservation authorized authentication")
	}
	if f.r.Pending != nil {
		t.Fatal("reservation failure created an intent")
	}
}

func TestRemoteRecoveryControlAuthAndAckAbsoluteDeadlines(t *testing.T) {
	t.Run("new authentication", func(t *testing.T) {
		f := newRecoveryControlFixture(t)
		f.start(t, recoveryFixtureID1, false, f.now)
		f.retire(t, f.now.Add(time.Second))
		s := f.status(t, true, f.now.Add(15*time.Minute))
		if s.Action != "none" || s.Code != "RECOVERY_BUDGET_EXPIRED" || s.AuthUsed != 1 {
			t.Fatalf("new auth deadline: %+v", s)
		}
		recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, true, f.now.Add(15*time.Minute)), "RECOVERY_BUDGET_EXPIRED")
	})
	t.Run("ACK", func(t *testing.T) {
		f := newRecoveryControlFixture(t)
		f.start(t, recoveryFixtureID1, false, f.now)
		f.commit(t, f.now.Add(14*time.Minute))
		firstFailure := f.now.Add(14*time.Minute + time.Second)
		if err := RecordRemoteRecoveryAckFailure(t.Context(), f.path, f.r, "HELPER_UNAVAILABLE", firstFailure); err != nil {
			t.Fatal(err)
		}
		// The original auth deadline does not truncate the independent ACK window.
		s := f.status(t, true, firstFailure.Add(time.Minute))
		if s.Action != "ack" || s.Code != "OK" {
			t.Fatalf("ACK inherited auth deadline: %+v", s)
		}
		before, _ := os.ReadFile(remoteRecoveryPath(f.path))
		if disabled := f.status(t, false, firstFailure.Add(15*time.Minute)); disabled.Action != "none" {
			t.Fatalf("disabled ACK scheduled: %+v", disabled)
		}
		after, _ := os.ReadFile(remoteRecoveryPath(f.path))
		if !bytes.Equal(before, after) {
			t.Fatal("disabled ACK reset deadline")
		}
		recoveryRequireCode(t, ReserveRemoteRecoveryAck(t.Context(), f.path, f.r, firstFailure.Add(15*time.Minute)), "RECOVERY_BUDGET_EXPIRED")
	})
}

func TestRemoteRecoveryControlCrashBeforeIntentCannotMintNewRound(t *testing.T) {
	f := newRecoveryControlFixture(t)
	if err := ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID1, false, f.now); err != nil {
		t.Fatal(err)
	}
	// Crash before pending persistence: there is no exact retired receipt, so
	// neither a fresh logical request nor a maintenance tick can mint a new ID.
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(context.Background(), f.path, f.r, recoveryFixtureID2, false, f.now.Add(time.Minute)), "RECOVERY_BUDGET_REQUIRED")
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(context.Background(), f.path, f.r, recoveryFixtureID2, true, f.now.Add(time.Minute)), "RECOVERY_BUDGET_REQUIRED")
	c, err := loadRemoteRecoveryControl(f.path)
	if err != nil || c.Round.AuthUsed != 1 {
		t.Fatalf("initial crash refunded reservation: %+v %v", c, err)
	}
}

func TestRemoteRecoveryControlCompletedAckCleanupJoinsNewReservation(t *testing.T) {
	f := newRecoveryControlFixture(t)
	f.start(t, recoveryFixtureID1, false, f.now)
	f.commit(t, f.now.Add(time.Minute))
	if err := RecordRemoteRecoveryAckFailure(t.Context(), f.path, f.r, "STATE_UNAVAILABLE", f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The native ACK clear CAS succeeded, but control cleanup was interrupted.
	f.r.Ack = nil
	state, err := LoadStorageState(f.path)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := state.Session()
	meta.Remote = f.r
	if state.SetSession(meta) != nil || WriteManagedSessionState(t.Context(), f.path, &state) != nil {
		t.Fatal("synthetic ACK clear CAS")
	}
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, true, f.now.Add(2*time.Minute)), "ACK_REQUIRED")
	f.r.RecoveryRequired = true
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, false, f.now.Add(2*time.Minute)), "ACK_REQUIRED")
	f.r.RecoveryRequired = false
	f.r.EnrollmentRequired = true
	recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, false, f.now.Add(2*time.Minute)), "ACK_REQUIRED")
	f.r.EnrollmentRequired = false
	f.start(t, recoveryFixtureID2, false, f.now.Add(2*time.Minute))
	c, err := loadRemoteRecoveryControl(f.path)
	if err != nil || c.Ack != nil || c.Round.FirstID != recoveryFixtureID2 || c.Round.AuthUsed != 1 {
		t.Fatalf("stale completed ACK blocked clean new reservation: %+v %v", c, err)
	}
}

func TestRemoteRecoveryControlNativeCompletionRequiresExactCommittedOperation(t *testing.T) {
	for _, mode := range []string{"committed", "crash-before-intent", "not-committed", "wrong-binding", "wrong-id", "recovery-required", "missing-signer", "save-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRecoveryControlFixture(t)
			if err := ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID1, false, f.now); err != nil {
				t.Fatal(err)
			}
			state, err := LoadStorageState(f.path)
			if err != nil {
				t.Fatal(err)
			}
			state.Cookies = append(state.Cookies,
				StateCookie{Name: "__Secure-1PAPISID", Value: "fixture", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
				StateCookie{Name: "__Secure-3PAPISID", Value: "fixture", Domain: ".google.com", Path: "/", Expires: -1, Secure: true})
			meta, _ := state.Session()
			if mode != "crash-before-intent" {
				meta.Remote.CookieRevision++
				meta.Remote.Ack = &SessionAck{ID: recoveryFixtureID1, Epoch: meta.Remote.Epoch, Cookie: meta.Remote.CookieRevision, Material: meta.Remote.MaterialVersion}
			}
			switch mode {
			case "wrong-binding":
				control, _ := loadRemoteRecoveryControl(f.path)
				control.Handle = "another_fixture_handle"
				if err := saveRemoteRecoveryControl(t.Context(), f.path, control); err != nil {
					t.Fatal(err)
				}
			case "wrong-id":
				meta.Remote.Ack.ID = recoveryFixtureID2
			case "recovery-required":
				meta.Remote.RecoveryRequired = true
			case "missing-signer":
				state.Cookies = nil
			}
			if err := state.SetSession(meta); err != nil {
				t.Fatal(err)
			}
			if mode != "not-committed" {
				if err := WriteManagedSessionState(t.Context(), f.path, &state); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(remoteRecoveryPath(f.path))
			ctx := t.Context()
			if mode == "save-failed" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			err = CompleteNativeRemoteRecoveryAuth(ctx, f.path, state)
			after, _ := os.ReadFile(remoteRecoveryPath(f.path))
			if mode == "committed" {
				c, loadErr := loadRemoteRecoveryControl(f.path)
				if err != nil || loadErr != nil || c.Round != nil || c.Ack == nil || c.Ack.ID != recoveryFixtureID1 {
					t.Fatal("exact native projection did not finish only its round", err, loadErr)
				}
				return
			}
			// A new binding ignores, but never closes, the former binding's budget.
			if mode == "wrong-binding" {
				if err != nil {
					t.Fatal("old control blocked an explicitly new binding", err)
				}
			} else if err == nil {
				t.Fatal("uncommitted, unrelated or invalid state finished the round")
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed native completion changed the persistent budget")
			}
			if mode == "crash-before-intent" {
				recoveryRequireCode(t, ReserveRemoteRecoveryAuth(t.Context(), f.path, f.r, recoveryFixtureID2, false, f.now.Add(time.Hour)), "RECOVERY_BUDGET_REQUIRED")
			}
		})
	}
}
