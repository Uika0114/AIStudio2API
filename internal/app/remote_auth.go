package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/authhelper"
)

type remoteRecoveryRequestKey struct{}

type remoteSessionProvider interface {
	PrepareRecovery(context.Context, *aistudio.StorageState, string, string, func(string) error) error
	AckSession(context.Context, *aistudio.RemoteSession) error
	Consumer() string
}

func (r *authRuntimeRefresher) configureRemote(path string) error {
	if path == "" {
		return nil
	}
	settings, err := authhelper.ReadClientSettings(path)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	client, err := authhelper.LoadClient(settings)
	if err != nil {
		return err
	}
	r.remote, r.closeRemote = client, client.Close
	return nil
}

func (r *authRuntimeRefresher) tryRemoteRefresh(ctx context.Context) (bool, error) {
	lease, ok := aistudio.AccountLeaseFromContext(ctx)
	if !ok {
		return false, nil
	}
	state, err := lease.ReloadStorageState()
	if err != nil {
		return false, nil
	} // Existing local path retains its original error handling.
	meta, err := state.Session()
	if err != nil {
		return true, aistudio.SessionError("INVALID_SESSION_STATE")
	}
	if meta == nil || meta.Remote == nil {
		return false, nil
	}
	if r.remote == nil {
		return true, aistudio.SessionError("REMOTE_DISABLED")
	}
	if ctx.Err() != nil {
		return true, aistudio.SessionError("CANCELED")
	}
	ctx = aistudio.WithRemoteAuthBudget(ctx)
	end, ok := lease.BeginAuthRefresh()
	if !ok {
		return true, aistudio.SessionError("BUSY")
	}
	defer end()
	if lease.WaitForAuthRefresh(ctx) != nil {
		return true, aistudio.SessionError("CANCELED")
	}
	account := lease.Account()
	ctx = authhelper.WithRecoveryObserver(ctx, func(v authhelper.RecoveryProgress) {
		if r.requests != nil {
			revision, lookup := "unknown", "not_applicable"
			if v.Revision != nil {
				revision = fmt.Sprint(*v.Revision)
			}
			if v.LookupOnly != nil {
				lookup = fmt.Sprint(*v.LookupOnly)
			}
			message := fmt.Sprintf("remote_auth phase=%s code=%s operation=%s revision=%s pending_revision=%d epoch=%d cookie_revision=%d material_version=%d lookup_only=%s", v.Phase, v.Code, v.OperationID, revision, v.PendingRevision, v.Epoch, v.CookieRevision, v.MaterialVersion, lookup)
			if id, _ := ctx.Value(remoteRecoveryRequestKey{}).(string); id != "" {
				r.requests.logRequestProgress(id, account.Config.Label, "INFO", message)
			} else {
				r.requests.log(account.Config.Label, "INFO", message)
			}
		}
	})
	err = lease.RefreshManagedStorageState(ctx, func(s *aistudio.StorageState) error {
		metadata, e := s.Session()
		if e != nil || metadata == nil || metadata.Remote == nil || metadata.Remote.Email != account.ID || metadata.Remote.Consumer != r.remote.Consumer() {
			return aistudio.SessionError("IDENTITY_MISMATCH")
		}
		if e = r.remote.PrepareRecovery(ctx, s, account.StoragePath, "reauth", func(id string) error { return aistudio.TakeRemoteAuthBudget(ctx, id) }); e != nil {
			return e
		}
		if _, e = aistudio.NewSigner().Sign(*s); e != nil {
			return aistudio.SessionError("COOKIE_POLICY_FAILURE")
		}
		return nil
	}, func() (func(bool), error) {
		if ctx.Err() != nil {
			return nil, aistudio.SessionError("CANCELED")
		}
		if r.reset == nil || r.prepareHeaders == nil {
			return nil, aistudio.SessionError("STATE_UNAVAILABLE")
		}
		if r.reset(account.ID) != nil {
			return nil, aistudio.SessionError("STATE_UNAVAILABLE")
		}
		return r.prepareHeaders(account.ID)
	})
	if err != nil {
		if !aistudio.IsSessionFailure(err) {
			err = aistudio.SessionError("STATE_UNAVAILABLE")
		}
		m, _ := state.Session()
		if current, e := lease.ReloadStorageState(); e == nil {
			m, _ = current.Session()
		}
		authhelper.ObserveRecoveryStage(ctx, "commit_blocked", err, m)
		return true, err
	}
	// Another lease may have completed a refresh while this lease waited.
	// The pool deduplicates that work without invoking our callback; do not
	// confuse a still-pending/retired recovery with a successful authentication.
	current, e := lease.ReloadStorageState()
	if e != nil {
		return true, aistudio.SessionError("STATE_UNAVAILABLE")
	}
	latest, e := current.Session()
	if e != nil || latest == nil || latest.Remote == nil || latest.Remote.Email != account.ID || latest.Remote.Consumer != r.remote.Consumer() {
		return true, aistudio.SessionError("IDENTITY_MISMATCH")
	}
	if latest.Remote.EnrollmentRequired {
		return true, aistudio.SessionError("BOOTSTRAP_REQUIRED")
	}
	if latest.Remote.Pending != nil || latest.Remote.RecoveryRequired {
		return true, aistudio.SessionError("VERSION_CONFLICT")
	}
	authhelper.ObserveRecoveryStage(ctx, "commit", nil, latest)
	// Cookies have committed. A lost ack must not turn the successful recovery
	// into a new auth workflow or discard the valid session. Next recovery retries it.
	ackErr := lease.UpdateRemoteSession(ctx, func(s *aistudio.SessionState) error {
		if s.Remote.Ack == nil {
			return nil
		}
		if err := r.remote.AckSession(ctx, s.Remote); err != nil {
			return err
		}
		s.Remote.Ack = nil
		return nil
	})
	if ackErr != nil {
		authhelper.ObserveRecoveryStage(ctx, "ack_pending", ackErr, latest)
	}
	return true, nil
}

func isRemoteLease(ctx context.Context) bool {
	lease, ok := aistudio.AccountLeaseFromContext(ctx)
	if !ok {
		return false
	}
	state, err := lease.ReloadStorageState()
	if err != nil {
		return false
	}
	meta, err := state.Session()
	return err != nil || meta != nil && meta.Remote != nil
}

func safeRemoteRecoveryFailure(err error) error {
	if aistudio.IsSessionFailure(err) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return aistudio.SessionError("CANCELED")
	}
	return aistudio.SessionError("STATE_UNAVAILABLE")
}
