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
	if err := ValidateCookies(state.Cookies, c.domains); err != nil {
		return err
	}
	request := operationRequest(r.Pending, r.Handle)
	request.LookupOnly = true
	result, err := c.rpc(ctx, "/v1/refresh", request)
	if err != nil {
		var notStarted *notStartedError
		if !errors.As(err, &notStarted) {
			return err
		}
		if aistudio.CheckSessionRevision(path, *state) != nil {
			return failure("VERSION_CONFLICT")
		}
		r.RetiredOperations = append(r.RetiredOperations, *r.Pending)
		r.Pending = nil
		r.EnrollmentRequired = false
		r.RecoveryRequired = false
		return state.SetSession(meta)
	}
	value := result.Session
	if value == nil || value.Version != Protocol || value.Handle != r.Handle || value.Epoch != r.Epoch || value.OperationID != request.OperationID || value.BaseCookie != r.CookieRevision || value.BaseMaterial != r.MaterialVersion || value.CookieRevision != r.CookieRevision+1 || value.MaterialVersion < r.MaterialVersion || value.MaterialVersion > r.MaterialVersion+1 || value.GaiaID != r.GaiaID || value.Email != r.Email {
		return failure("IDENTITY_MISMATCH")
	}
	if err := ValidateCookies(value.Cookies, c.domains); err != nil {
		return err
	}
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
