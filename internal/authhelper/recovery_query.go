package authhelper

import (
	"context"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// RecoveryProgress contains only bounded protocol metadata, never cookies,
// source paths, credentials, underlying network errors or their fingerprints.
type RecoveryProgress struct {
	Phase           string
	Code            string
	OperationID     string
	Revision        *uint64
	PendingRevision uint64
	Epoch           uint64
	CookieRevision  uint64
	MaterialVersion uint64
	// LookupOnly is applicable only to the refresh RPC query result, not
	// a commit, ACK or all authentication work of this operation.
	LookupOnly *bool
}
type recoveryObserverKey struct{}

// WithRecoveryObserver connects native/app diagnostics without making ordinary
// cookie writers depend on a logger or changing the v1 helper wire protocol.
func WithRecoveryObserver(ctx context.Context, observe func(RecoveryProgress)) context.Context {
	return context.WithValue(ctx, recoveryObserverKey{}, observe)
}
func reportRecovery(ctx context.Context, phase, code string, meta *aistudio.SessionState, lookup bool) {
	observe, _ := ctx.Value(recoveryObserverKey{}).(func(RecoveryProgress))
	if observe == nil {
		return
	}
	v := RecoveryProgress{Phase: phase, Code: code}
	if phase == "query" {
		value := lookup
		v.LookupOnly = &value
	}
	if meta != nil && meta.Remote != nil {
		r := meta.Remote
		if meta.Revision != 0 {
			value := meta.Revision
			v.Revision = &value
		}
		v.Epoch = r.Epoch
		v.CookieRevision = r.CookieRevision
		v.MaterialVersion = r.MaterialVersion
		if r.Pending != nil {
			v.OperationID = r.Pending.ID
			v.PendingRevision = r.Pending.StateRevision
		} else if r.Ack != nil {
			v.OperationID = r.Ack.ID
		}
	}
	observe(v)
}

// ObserveRecoveryStage lets the runtime/native caller report the durable result
// of its own commit or ACK boundary using the same safe-code allowlist.
func ObserveRecoveryStage(ctx context.Context, phase string, err error, meta *aistudio.SessionState) {
	code := "OK"
	if err != nil {
		code = safeCode(err)
	}
	reportRecovery(ctx, phase, code, meta, true)
}

// pendingResult observes the immutable intent using the current local snapshot.
// A local revision mismatch can block projection acceptance, but not lookup.
func (c *Client) pendingResult(ctx context.Context, state *aistudio.StorageState, path string, meta *aistudio.SessionState, lookup bool) (*Result, error) {
	if ctx.Err() != nil {
		return nil, failure("CANCELED")
	}
	if meta.Remote.Pending == nil {
		return nil, failure("INVALID_REQUEST")
	}
	if err := aistudio.CheckSessionRevision(path, *state); err != nil {
		reportRecovery(ctx, "snapshot", "VERSION_CONFLICT", meta, lookup)
		return nil, err
	}
	if meta.Remote.Pending.StateRevision != meta.Revision {
		reportRecovery(ctx, "pending_baseline", "VERSION_CONFLICT", meta, lookup)
	}
	request := operationRequest(meta.Remote.Pending, meta.Remote.Handle)
	request.LookupOnly = lookup
	result, err := c.rpc(ctx, "/v1/refresh", request)
	if ctx.Err() != nil {
		reportRecovery(ctx, "query", "CANCELED", meta, lookup)
		return nil, failure("CANCELED")
	}
	code := "OK"
	if err != nil {
		code = safeCode(err)
	}
	reportRecovery(ctx, "query", code, meta, lookup)
	return result, err
}

// retireNotStarted is not a recovery: preserve cookies, control versions and the
// enrollment fence. The caller may persist this metadata-only disposition under
// its current snapshot CAS; the original operation's StateRevision is unchanged.
func retireNotStarted(ctx context.Context, state *aistudio.StorageState, path string, meta *aistudio.SessionState, receipt RefreshRequest, keepCurrent bool) error {
	if ctx.Err() != nil {
		return failure("CANCELED")
	}
	r := meta.Remote
	if r.Pending == nil || operationRequest(r.Pending, r.Handle) != withoutLookup(receipt) {
		return failure("IDENTITY_MISMATCH")
	}
	if err := aistudio.CheckSessionRevision(path, *state); err != nil {
		reportRecovery(ctx, "retire", "VERSION_CONFLICT", meta, true)
		return err
	}
	// Do not leave caller memory claiming a retirement that failed to persist.
	candidate, err := cloneState(*state)
	if err != nil {
		return err
	}
	next, err := candidate.Session()
	if err != nil {
		return failure("INVALID_SESSION_STATE")
	}
	next.Remote.RetiredOperations = append(next.Remote.RetiredOperations, *next.Remote.Pending)
	next.Remote.Pending = nil
	// Explicit reauth disposition can choose the existing enrolled session;
	// no-send proof never completes bootstrap or proves cookie validity.
	next.Remote.RecoveryRequired = !(keepCurrent && !r.EnrollmentRequired && receipt.Reason == "reauth")
	if err = candidate.SetSession(next); err != nil {
		return err
	}
	if err = aistudio.WriteManagedSessionState(ctx, path, &candidate); err != nil {
		reportRecovery(ctx, "retire", safeCode(err), meta, true)
		return failure(safeCode(err))
	}
	*state = candidate
	reportRecovery(ctx, "retire", "NOT_STARTED", meta, true)
	return nil
}
func withoutLookup(r RefreshRequest) RefreshRequest { r.LookupOnly = false; return r }

func (c *Client) validateRecoveryResult(meta *aistudio.SessionState, request RefreshRequest, result *Result) error {
	r := meta.Remote
	if result == nil {
		return failure("IDENTITY_MISMATCH")
	}
	v := result.Session
	if v == nil || v.Version != Protocol || v.Handle != r.Handle || v.Epoch != r.Epoch || v.OperationID != request.OperationID || v.BaseCookie != r.CookieRevision || v.BaseMaterial != r.MaterialVersion || v.CookieRevision != r.CookieRevision+1 || v.MaterialVersion < r.MaterialVersion || v.MaterialVersion > r.MaterialVersion+1 || v.GaiaID != r.GaiaID || v.Email != r.Email {
		return failure("IDENTITY_MISMATCH")
	}
	return ValidateCookies(v.Cookies, c.domains)
}
