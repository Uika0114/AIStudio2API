package authhelper

import (
	"context"
	"errors"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// ReconcileKeepingCurrent is an explicit native-operator disposition, never a
// generation retry: preserve independently updated local cookies, retire the
// pending helper result, and acknowledge control versions as keep_current.
func (c *Client) ReconcileKeepingCurrent(ctx context.Context, state *aistudio.StorageState, path string) error {
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return failure("IDENTITY_MISMATCH")
	}
	r := meta.Remote
	if r.Consumer != c.consumer || r.Pending == nil || r.Pending.StateRevision == meta.Revision || r.Ack != nil {
		return failure("INVALID_REQUEST")
	}
	if !r.EnrollmentRequired && r.Pending.Reason != "bootstrap" {
		if err := ValidateCookies(state.Cookies, c.domains); err != nil {
			return err
		}
	}
	request := operationRequest(r.Pending, r.Handle)
	request.LookupOnly = true
	result, err := c.pendingResult(ctx, state, path, meta, true)
	if err != nil {
		var notStarted *notStartedError
		if !errors.As(err, &notStarted) {
			return err
		}
		if e := retireNotStarted(ctx, state, path, meta, notStarted.receipt, true); e != nil {
			return e
		}
		// No-send disposition was committed under the current snapshot CAS.
		// A bootstrap remains unfinished even if local cookies happen to sign.
		if r.EnrollmentRequired || request.Reason == "bootstrap" {
			return err
		}
		return nil
	}
	if err := c.validateRecoveryResult(meta, request, result); err != nil {
		return err
	}
	// Choosing existing cookies is never a substitute for receiving the first
	// projection of a newly enrolled binding, even when a ready result exists.
	if r.EnrollmentRequired || request.Reason == "bootstrap" {
		return failure("BOOTSTRAP_REQUIRED")
	}
	if err := ValidateCookies(state.Cookies, c.domains); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return failure("CANCELED")
	}
	value := result.Session
	if err := aistudio.CheckSessionRevision(path, *state); err != nil {
		return err
	}
	r.RetiredOperations = append(r.RetiredOperations, *r.Pending)
	r.Pending = nil
	r.RecoveryRequired = false
	r.CookieRevision = value.CookieRevision
	r.MaterialVersion = value.MaterialVersion
	r.EnrollmentRequired = false
	r.Ack = &aistudio.SessionAck{ID: request.OperationID, Epoch: r.Epoch, Cookie: r.CookieRevision, Material: r.MaterialVersion, Mode: "keep_current"}
	return state.SetSession(meta)
}
