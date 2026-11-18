package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"time"
)

const remoteRecoveryControlVersion = 1

// The account is identified by the adjacent storage path. This file contains
// only scheduling limits, never cookies, OAuth material or authentication truth.
type remoteRecoveryControl struct {
	Version int                  `json:"version"`
	Handle  string               `json:"handle"`
	Epoch   uint64               `json:"epoch"`
	Round   *remoteRecoveryRound `json:"round,omitempty"`
	Ack     *remoteRecoveryAck   `json:"ack,omitempty"`
}

type remoteRecoveryRound struct {
	FirstID  string              `json:"first_id"`
	Current  string              `json:"current_id"`
	Started  time.Time           `json:"started"`
	Deadline time.Time           `json:"deadline"`
	AuthUsed int                 `json:"auth_used"`
	Retired  bool                `json:"retired,omitempty"`
	NextAuth time.Time           `json:"next_auth,omitempty"`
	Pending  remoteRecoveryRetry `json:"pending"`
}

type remoteRecoveryRetry struct {
	Used     int       `json:"used"`
	Queries  int       `json:"queries,omitempty"`
	Commits  int       `json:"commits,omitempty"`
	Next     time.Time `json:"next,omitempty"`
	InFlight bool      `json:"in_flight,omitempty"`
	Stop     string    `json:"stop,omitempty"`
}

type remoteRecoveryAck struct {
	ID       string              `json:"id"`
	Started  time.Time           `json:"started,omitempty"`
	Deadline time.Time           `json:"deadline,omitempty"`
	Retry    remoteRecoveryRetry `json:"retry"`
}

// RemoteRecoveryStatus is a read-only budget projection, not account health.
// Action is none, new_auth, pending or ack. An action is runnable only when Code
// is OK; BACKOFF retains Action and Due so the native lifecycle can schedule it.
type RemoteRecoveryStatus struct {
	Action      string
	Due         time.Time
	Code        string
	AuthUsed    int
	PendingUsed int
	Queries     int
	Commits     int
	AckUsed     int
	Deadline    time.Time
}

func remoteRecoveryPath(path string) string { return path + ".recovery-control.json" }

func loadRemoteRecoveryControl(path string) (*remoteRecoveryControl, error) {
	if path == "" {
		return nil, SessionError("RECOVERY_CONTROL_UNAVAILABLE")
	}
	file, err := os.Open(remoteRecoveryPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	if err != nil {
		return nil, SessionError("RECOVERY_CONTROL_UNAVAILABLE")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return nil, SessionError("RECOVERY_CONTROL_UNAVAILABLE")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var control remoteRecoveryControl
	if decoder.Decode(&control) != nil || decoder.Decode(new(any)) != io.EOF || !control.valid() {
		return nil, SessionError("RECOVERY_CONTROL_INVALID")
	}
	return &control, nil
}

func (c *remoteRecoveryControl) valid() bool {
	if c.Version != remoteRecoveryControlVersion || !ValidSessionOperationID(c.Handle) || c.Epoch == 0 {
		return false
	}
	if r := c.Round; r != nil {
		if !ValidSessionOperationID(r.FirstID) || !ValidSessionOperationID(r.Current) ||
			r.Started.IsZero() || !r.Deadline.Equal(r.Started.Add(15*time.Minute)) ||
			r.AuthUsed < 1 || r.AuthUsed > 3 || !r.Pending.valid() ||
			r.Pending.Queries != r.Pending.Used || r.Retired && r.NextAuth.IsZero() {
			return false
		}
	}
	if a := c.Ack; a != nil {
		if !ValidSessionOperationID(a.ID) || !a.Retry.valid() || a.Retry.Queries != 0 || a.Retry.Commits != 0 {
			return false
		}
		if a.Started.IsZero() {
			if !a.Deadline.IsZero() || a.Retry.Used != 0 || !a.Retry.Next.IsZero() || a.Retry.InFlight {
				return false
			}
		} else if !a.Deadline.Equal(a.Started.Add(15 * time.Minute)) {
			return false
		}
	}
	return true
}

func (r remoteRecoveryRetry) valid() bool {
	return r.Used >= 0 && r.Used <= 3 && r.Queries >= 0 && r.Queries <= r.Used &&
		r.Commits >= 0 && r.Commits <= r.Used && (!r.InFlight || r.Used > 0) &&
		(r.Stop == "" || remoteRecoveryStopCode(r.Stop) == r.Stop)
}

func remoteRecoveryStopCode(code string) string {
	switch code {
	case "IDENTITY_MISMATCH", "VERSION_CONFLICT", "OPERATION_CONFLICT", "COOKIE_POLICY_FAILURE",
		"INVALID_SESSION_STATE", "INVALID_REQUEST", "MATERIAL_REJECTED", "SOURCE_REBIND_REQUIRED",
		"UNSUPPORTED_KEY", "REMOTE_OWNED", "MAPPING_DISABLED":
		return code
	}
	return ""
}

func saveRemoteRecoveryControl(ctx context.Context, path string, c *remoteRecoveryControl) error {
	if ctx.Err() != nil {
		return SessionError("CANCELED")
	}
	if path == "" || !c.valid() {
		return SessionError("RECOVERY_CONTROL_INVALID")
	}
	data, err := json.Marshal(c)
	if err != nil || atomicWriteFile(remoteRecoveryPath(path), append(data, '\n'), 0o600) != nil {
		return SessionError("RECOVERY_CONTROL_UNAVAILABLE")
	}
	// atomicWriteFile syncs the new file. Persist the rename on platforms that
	// support directory fsync as well; failures never authorize an action.
	if runtime.GOOS != "windows" {
		directory, err := os.Open(filepath.Dir(remoteRecoveryPath(path)))
		if err != nil {
			return SessionError("RECOVERY_CONTROL_UNAVAILABLE")
		}
		err = directory.Sync()
		closeErr := directory.Close()
		if err != nil || closeErr != nil {
			return SessionError("RECOVERY_CONTROL_UNAVAILABLE")
		}
	}
	return nil
}

func remoteRecoveryFor(path string, r *RemoteSession) (*remoteRecoveryControl, error) {
	c, err := loadRemoteRecoveryControl(path)
	if err != nil {
		return nil, err
	}
	if r == nil || c.Handle != r.Handle || c.Epoch != r.Epoch {
		return nil, SessionError("RECOVERY_BINDING_MISMATCH")
	}
	return c, nil
}

// All mutating functions below require the caller's original account file lease
// and storage lock. They deliberately do not acquire a competing lock, and must
// be called before the associated RPC/commit. now is supplied by the lifecycle,
// allowing deterministic clocks without a process-global clock override.

// ReserveRemoteRecoveryAuth spends the initial attempt before pending is saved.
// Only an explicitly foreground, clean new intent may establish a round. A
// background intent requires the preceding exact NOT_STARTED retirement.
func ReserveRemoteRecoveryAuth(ctx context.Context, path string, r *RemoteSession, operationID string, background bool, now time.Time) error {
	if r == nil || !ValidSessionOperationID(r.Handle) || r.Epoch == 0 || !ValidSessionOperationID(operationID) || now.IsZero() {
		return SessionError("INVALID_SESSION_STATE")
	}
	if r.Pending != nil || r.Ack != nil {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	c, err := loadRemoteRecoveryControl(path)
	clean := !r.RecoveryRequired
	if err != nil && !IsSessionFailureCode(err, "RECOVERY_BUDGET_REQUIRED") {
		return err
	}
	if c == nil || c.Handle != r.Handle || c.Epoch != r.Epoch {
		if background || !clean {
			if c != nil {
				return SessionError("RECOVERY_BINDING_MISMATCH")
			}
			return SessionError("RECOVERY_BUDGET_REQUIRED")
		}
		c = &remoteRecoveryControl{Version: remoteRecoveryControlVersion, Handle: r.Handle, Epoch: r.Epoch}
	}
	if c.Ack != nil {
		// The native ACK clear CAS may have succeeded while its control cleanup
		// failed. Only a clean, explicit new round may retire that stale slot;
		// its removal and the new reservation share one atomic replacement.
		if background || !clean || r.EnrollmentRequired || c.Round != nil {
			return SessionError("ACK_REQUIRED")
		}
		c.Ack = nil
	}
	if c.Round == nil {
		if background || !clean {
			return SessionError("RECOVERY_BUDGET_REQUIRED")
		}
		c.Round = &remoteRecoveryRound{FirstID: operationID, Current: operationID, Started: now, Deadline: now.Add(15 * time.Minute), AuthUsed: 1}
		return saveRemoteRecoveryControl(ctx, path, c)
	}
	b := c.Round
	if !background || !b.Retired || b.Current == operationID || b.FirstID == operationID {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	if code := remoteRecoveryLimit(b.AuthUsed, b.Deadline, b.NextAuth, now, b.Pending.Stop); code != "OK" {
		return SessionError(code)
	}
	b.AuthUsed++
	b.Current, b.Retired, b.NextAuth = operationID, false, time.Time{}
	b.Pending = remoteRecoveryRetry{}
	return saveRemoteRecoveryControl(ctx, path, c)
}

// RecordRemoteRecoveryFailure registers only a pending ID with a proven budget.
// Repeated callers cannot shift its first due time or replenish its attempts.
// code carries a fixed protocol failure, never an underlying error.
func RecordRemoteRecoveryFailure(ctx context.Context, path string, r *RemoteSession, code string, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Round == nil || r.Pending == nil || c.Round.Current != r.Pending.ID || c.Round.Retired {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	p := &c.Round.Pending
	if remoteRecoveryStopCode(code) != "" {
		p.Stop = remoteRecoveryStopCode(code)
	}
	if p.Next.IsZero() {
		p.Next = now.Add(30 * time.Second)
	}
	return saveRemoteRecoveryControl(ctx, path, c)
}

// RetireRemoteRecoveryNotStarted follows, never precedes, the native durable
// retirement of the exact intent after validating the NOT_STARTED receipt.
func RetireRemoteRecoveryNotStarted(ctx context.Context, path string, r *RemoteSession, operation SessionOperation, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Round == nil || c.Round.Current != operation.ID || r.Pending != nil || !r.RecoveryRequired {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	found := false
	for _, retired := range r.RetiredOperations {
		if reflect.DeepEqual(retired, operation) {
			found = true
			break
		}
	}
	if !found || operation.Epoch != r.Epoch {
		return SessionError("IDENTITY_MISMATCH")
	}
	b := c.Round
	if !b.Retired {
		delay := time.Minute
		if b.AuthUsed >= 2 {
			delay = 5 * time.Minute
		}
		b.Retired, b.NextAuth = true, now.Add(delay)
		b.Pending.InFlight = false
	}
	return saveRemoteRecoveryControl(ctx, path, c)
}

func remoteRecoveryLimit(used int, deadline, due, now time.Time, stop string) string {
	if stop != "" {
		return stop
	}
	if !now.Before(deadline) {
		return "RECOVERY_BUDGET_EXPIRED"
	}
	if used >= 3 {
		return "RECOVERY_BUDGET_EXHAUSTED"
	}
	if due.IsZero() {
		return "RECOVERY_BUDGET_REQUIRED"
	}
	if now.Before(due) {
		return "RECOVERY_BACKOFF"
	}
	return "OK"
}

func remoteRecoveryDelay(used int) time.Duration {
	if used < 2 {
		return 2 * time.Minute
	}
	return 5 * time.Minute
}

func reserveRemoteRecoveryRetry(ctx context.Context, path string, c *remoteRecoveryControl, p *remoteRecoveryRetry, deadline, now time.Time, query bool) error {
	if code := remoteRecoveryLimit(p.Used, deadline, p.Next, now, p.Stop); code != "OK" {
		return SessionError(code)
	}
	if p.InFlight {
		// The former lease did not finish its reservation. Spend that slot and
		// start a fresh quiet interval instead of catching up overdue ticks.
		p.InFlight, p.Next = false, now.Add(remoteRecoveryDelay(p.Used))
		if err := saveRemoteRecoveryControl(ctx, path, c); err != nil {
			return err
		}
		return SessionError("RECOVERY_BACKOFF")
	}
	p.Used++
	if query {
		p.Queries++
	}
	p.InFlight, p.Next = true, now.Add(remoteRecoveryDelay(p.Used))
	return saveRemoteRecoveryControl(ctx, path, c)
}

// ReserveRemoteRecoveryPending authorizes one lookup and, if valid, at most one
// projection commit in that background round. A failed save authorizes neither.
func ReserveRemoteRecoveryPending(ctx context.Context, path string, r *RemoteSession, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Round == nil || r.Pending == nil || c.Round.Current != r.Pending.ID || c.Round.Retired {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	return reserveRemoteRecoveryRetry(ctx, path, c, &c.Round.Pending, c.Round.Deadline, now, true)
}

func FinishRemoteRecoveryPending(ctx context.Context, path string, r *RemoteSession, operationID string, submitted bool, code string, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Round == nil || c.Round.Current != operationID {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	p := &c.Round.Pending
	if !p.InFlight {
		return nil
	}
	if submitted {
		p.Commits++
	}
	if remoteRecoveryStopCode(code) != "" {
		p.Stop = remoteRecoveryStopCode(code)
	}
	p.InFlight, p.Next = false, now.Add(remoteRecoveryDelay(p.Used))
	return saveRemoteRecoveryControl(ctx, path, c)
}

// CompleteRemoteRecoveryAuth may only be called after durable projection AND
// runtime adoption. Session metadata alone is not proof of runtime adoption.
// Keep the current ACK's provenance after the authentication round ends.
func CompleteRemoteRecoveryAuth(ctx context.Context, path string, r *RemoteSession, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	return completeRemoteRecoveryAuth(ctx, path, r, c)
}

func completeRemoteRecoveryAuth(ctx context.Context, path string, r *RemoteSession, c *remoteRecoveryControl) error {
	if r.Pending != nil || r.RecoveryRequired || r.EnrollmentRequired {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	if c.Round == nil {
		if r.Ack == nil || c.Ack != nil && c.Ack.ID == r.Ack.ID {
			return nil
		}
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	if r.Ack == nil || r.Ack.ID != c.Round.Current || r.Ack.Epoch != r.Epoch {
		return SessionError("IDENTITY_MISMATCH")
	}
	if c.Ack != nil && c.Ack.ID != r.Ack.ID {
		return SessionError("ACK_REQUIRED")
	}
	if c.Ack == nil {
		c.Ack = &remoteRecoveryAck{ID: r.Ack.ID}
	}
	c.Round = nil
	return saveRemoteRecoveryControl(ctx, path, c)
}

// nativeRetiredReauth identifies an exact prior intent in the current binding's
// durable retirement history. A keep_current disposition also retains the raw
// intent, but advances the control versions; callers must check that base.
func nativeRetiredReauth(c *remoteRecoveryControl, meta *SessionState) *SessionOperation {
	if c.Round.Pending.Commits != 0 {
		return nil
	}
	var found *SessionOperation
	for i := range meta.Remote.RetiredOperations {
		op := &meta.Remote.RetiredOperations[i]
		if op.ID != c.Round.Current {
			continue
		}
		if found != nil || !validOperation(op) || op.Reason != "reauth" || op.Epoch != meta.Remote.Epoch ||
			op.StateRevision == 0 || op.StateRevision >= meta.Revision || op.Stage != "" ||
			op.ResultCookie != 0 || op.ResultMaterial != 0 || op.Failure != "" || op.HTTP != nil || op.AckMode != "" {
			return nil
		}
		found = op
	}
	return found
}

// CheckNativeRemoteReauth permits a new explicit reauth against an existing
// same-binding round only after the old intent was durably retired. It neither
// reserves nor reopens automatic recovery. The native caller holds the lease
// and passes the session it just read from disk.
func CheckNativeRemoteReauth(path string, meta *SessionState) error {
	if meta == nil || meta.Remote == nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	r := meta.Remote
	if r.Pending != nil || r.Ack != nil || r.EnrollmentRequired || !r.RecoveryRequired {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	c, err := remoteRecoveryFor(path, r)
	if IsSessionFailureCode(err, "RECOVERY_BUDGET_REQUIRED") || IsSessionFailureCode(err, "RECOVERY_BINDING_MISMATCH") {
		return nil
	}
	if err != nil {
		return err
	}
	if c.Ack != nil {
		return SessionError("ACK_REQUIRED")
	}
	if c.Round != nil {
		old := nativeRetiredReauth(c, meta)
		if old == nil || old.BaseCookie != r.CookieRevision || old.BaseMaterial != r.MaterialVersion {
			return SessionError("RECOVERY_BUDGET_REQUIRED")
		}
	}
	return nil
}

// CompleteNativeRemoteRecoveryAuth closes only the budget for an explicitly
// requested native projection commit. It does not publish pool readiness or
// stand in for consumer runtime adoption. The caller holds the account lease.
// Keep ACK provenance until after native ACK persistence; a crash before this
// save leaves the exact ACK available for an explicit retry, never a clean-state
// inference. Legacy or differently bound controls are left untouched.
func CompleteNativeRemoteRecoveryAuth(ctx context.Context, path string, state StorageState) error {
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	persisted, err := LoadStorageState(path)
	if err != nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	actual, err := persisted.Session()
	if err != nil || !reflect.DeepEqual(actual, meta) {
		return SessionError("VERSION_CONFLICT")
	}
	if _, err := NewSigner().Sign(persisted); err != nil {
		return SessionError("COOKIE_POLICY_FAILURE")
	}
	c, err := remoteRecoveryFor(path, meta.Remote)
	if IsSessionFailureCode(err, "RECOVERY_BUDGET_REQUIRED") || IsSessionFailureCode(err, "RECOVERY_BINDING_MISMATCH") {
		return nil
	}
	if err != nil {
		return err
	}
	r := meta.Remote
	if c.Round == nil {
		if c.Ack != nil && (r.Ack == nil || c.Ack.ID != r.Ack.ID) {
			return SessionError("ACK_REQUIRED")
		}
		return nil
	}
	if r.Ack == nil || r.Ack.ID == c.Round.Current {
		return completeRemoteRecoveryAuth(ctx, path, r, c)
	}
	// An already committed native successor may be acknowledged even if its
	// old automatic round was never marked retired in the control file. The
	// proof is the exact durable raw intent plus the adjacent committed
	// projection, not an expired deadline or merely an ACK with another ID.
	if c.Ack != nil {
		return SessionError("ACK_REQUIRED")
	}
	// The old intent, its retirement, the new intent and its commit are four
	// distinct durable revisions; unrelated writes can widen, not shrink, the gap.
	old := nativeRetiredReauth(c, meta)
	if old == nil || r.Pending != nil || r.RecoveryRequired || r.EnrollmentRequired || r.Ack.Mode != "" ||
		meta.Revision-old.StateRevision < 3 || r.CookieRevision == 0 || r.CookieRevision-1 != old.BaseCookie ||
		r.MaterialVersion < old.BaseMaterial || r.MaterialVersion-old.BaseMaterial > 1 {
		return SessionError("IDENTITY_MISMATCH")
	}
	for _, retired := range r.RetiredOperations {
		if retired.ID == r.Ack.ID {
			return SessionError("IDENTITY_MISMATCH")
		}
	}
	// Close the old round and stage only the exact new ACK, atomically. No
	// retry budget is minted and the session/history stays untouched here.
	c.Round = nil
	c.Ack = &remoteRecoveryAck{ID: r.Ack.ID}
	return saveRemoteRecoveryControl(ctx, path, c)
}

// The caller invokes the first ACK only as part of successful adoption. Other
// callers must reserve compensation, never repeat the uncounted first attempt.
// A crash before recording its failure leaves no automatic ACK allowance.
func RecordRemoteRecoveryAckFailure(ctx context.Context, path string, r *RemoteSession, code string, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Ack == nil || r.Ack == nil || c.Ack.ID != r.Ack.ID {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	if c.Ack.Started.IsZero() {
		c.Ack.Started, c.Ack.Deadline = now, now.Add(15*time.Minute)
		c.Ack.Retry.Next = now.Add(30 * time.Second)
	}
	if remoteRecoveryStopCode(code) != "" {
		c.Ack.Retry.Stop = remoteRecoveryStopCode(code)
	}
	return saveRemoteRecoveryControl(ctx, path, c)
}

func ReserveRemoteRecoveryAck(ctx context.Context, path string, r *RemoteSession, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Ack == nil || r.Ack == nil || c.Ack.ID != r.Ack.ID || c.Ack.Started.IsZero() {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	return reserveRemoteRecoveryRetry(ctx, path, c, &c.Ack.Retry, c.Ack.Deadline, now, false)
}

func FinishRemoteRecoveryAck(ctx context.Context, path string, r *RemoteSession, operationID string, code string, now time.Time) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if c.Ack == nil || c.Ack.ID != operationID {
		return SessionError("RECOVERY_BUDGET_REQUIRED")
	}
	p := &c.Ack.Retry
	if !p.InFlight {
		return nil
	}
	if remoteRecoveryStopCode(code) != "" {
		p.Stop = remoteRecoveryStopCode(code)
	}
	p.InFlight, p.Next = false, now.Add(remoteRecoveryDelay(p.Used))
	return saveRemoteRecoveryControl(ctx, path, c)
}

// CompleteRemoteRecoveryAck follows a successful local clear CAS. It does not
// discard a still-persistent ACK, even when the remote RPC already succeeded.
func CompleteRemoteRecoveryAck(ctx context.Context, path string, r *RemoteSession, operationID string) error {
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		return err
	}
	if r.Ack != nil {
		return SessionError("ACK_REQUIRED")
	}
	if c.Ack == nil {
		return nil
	}
	if c.Ack.ID != operationID {
		return SessionError("IDENTITY_MISMATCH")
	}
	c.Ack = nil
	return saveRemoteRecoveryControl(ctx, path, c)
}

// ReadRemoteRecoveryStatus never creates or repairs a control file. Disabled
// merely masks automatic eligibility; budgets and absolute deadlines survive.
func ReadRemoteRecoveryStatus(path string, r *RemoteSession, enabled bool, now time.Time) (RemoteRecoveryStatus, error) {
	s := RemoteRecoveryStatus{Action: "none", Code: "RECOVERY_BUDGET_REQUIRED"}
	c, err := remoteRecoveryFor(path, r)
	if err != nil {
		var failure *SessionFailure
		if errors.As(err, &failure) {
			s.Code = failure.Code
		}
		return s, err
	}
	if b := c.Round; b != nil {
		s.AuthUsed, s.PendingUsed, s.Queries, s.Commits = b.AuthUsed, b.Pending.Used, b.Pending.Queries, b.Pending.Commits
		s.Deadline = b.Deadline
		if r.Pending != nil && r.Pending.ID == b.Current && !b.Retired {
			s.Action, s.Due = "pending", b.Pending.Next
			s.Code = remoteRecoveryLimit(b.Pending.Used, b.Deadline, s.Due, now, b.Pending.Stop)
		} else if r.Pending == nil && r.Ack == nil && b.Retired && r.RecoveryRequired {
			s.Action, s.Due = "new_auth", b.NextAuth
			s.Code = remoteRecoveryLimit(b.AuthUsed, b.Deadline, s.Due, now, b.Pending.Stop)
		}
	}
	if a := c.Ack; a != nil {
		s.AckUsed = a.Retry.Used
		if r.Ack != nil && r.Ack.ID == a.ID {
			s.Action, s.Due, s.Deadline = "ack", a.Retry.Next, a.Deadline
			if a.Started.IsZero() {
				s.Code = "RECOVERY_BUDGET_REQUIRED"
			} else {
				s.Code = remoteRecoveryLimit(a.Retry.Used, a.Deadline, s.Due, now, a.Retry.Stop)
			}
		}
	}
	if c.Round == nil && c.Ack == nil && r.Pending == nil && r.Ack == nil && !r.RecoveryRequired && !r.EnrollmentRequired {
		s.Code = "OK"
	}
	if !enabled {
		s.Action, s.Code = "none", "ACCOUNT_DISABLED"
	} else if s.Code != "OK" && s.Code != "RECOVERY_BACKOFF" {
		s.Action = "none"
	}
	return s, nil
}
