package app

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	}
	meta, err := state.Session()
	if err != nil {
		return true, aistudio.SessionError("INVALID_SESSION_STATE")
	}
	if meta == nil || meta.Remote == nil {
		return false, nil
	}
	if !aistudio.RemoteAuthenticationAllowed(ctx) {
		return true, aistudio.SessionError("RECOVERY_NOT_ALLOWED")
	}
	if r.remote == nil {
		return true, aistudio.SessionError("REMOTE_DISABLED")
	}
	if ctx.Err() != nil {
		return true, aistudio.SessionError("CANCELED")
	}
	ctx = authhelper.WithRecoveryControl(aistudio.WithRemoteAuthBudget(ctx), r.recoveryNow)
	end, ok := lease.BeginAuthRefresh()
	if !ok {
		return true, aistudio.SessionError("BUSY")
	}
	defer end()
	if lease.WaitForAuthRefresh(ctx) != nil {
		return true, aistudio.SessionError("CANCELED")
	}
	account := lease.Account()
	defer func() {
		// Publish before the triggering generation lease is released. Cancellation
		// does not erase an intent which was already durably recorded.
		_ = lease.RefreshRemoteQuarantine()
		r.registerRemoteRecovery(account.ID)
		r.publishRecoveryAccounts()
	}()
	ctx = r.observeRemoteRecovery(ctx, account)
	if meta.Remote.Ack != nil {
		if err := r.retryRemoteAck(ctx, lease, false); err != nil {
			return true, err
		}
	}
	background, _ := ctx.Value(remoteMaintenanceKey{}).(bool)
	pendingID, submitted, prepared := "", false, false
	err = lease.RefreshManagedStorageState(ctx, func(s *aistudio.StorageState) error {
		metadata, e := s.Session()
		if e != nil || metadata == nil || metadata.Remote == nil || metadata.Remote.Email != account.ID || metadata.Remote.Consumer != r.remote.Consumer() {
			return aistudio.SessionError("IDENTITY_MISMATCH")
		}
		if metadata.Remote.Pending != nil {
			if e = aistudio.ReserveRemoteRecoveryPending(ctx, account.StoragePath, metadata.Remote, r.recoveryNow()); e != nil {
				return e
			}
			pendingID = metadata.Remote.Pending.ID
		}
		if e = r.remote.PrepareRecovery(ctx, s, account.StoragePath, "reauth", func(id string) error {
			if e := aistudio.TakeRemoteAuthBudget(ctx, id); e != nil {
				return e
			}
			return aistudio.ReserveRemoteRecoveryAuth(ctx, account.StoragePath, metadata.Remote, id, background, r.recoveryNow())
		}); e != nil {
			return e
		}
		if _, e = aistudio.NewSigner().Sign(*s); e != nil {
			return aistudio.SessionError("COOKIE_POLICY_FAILURE")
		}
		prepared = true
		return nil
	}, func() (func(bool), error) {
		if ctx.Err() != nil {
			return nil, aistudio.SessionError("CANCELED")
		}
		if r.reset == nil || r.prepareHeaders == nil || r.reset(account.ID) != nil {
			return nil, aistudio.SessionError("STATE_UNAVAILABLE")
		}
		finish, e := r.prepareHeaders(account.ID)
		submitted = e == nil
		return finish, e
	})
	// This context is used for metadata only, never a new RPC after cancellation.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if pendingID != "" {
		finishErr := lease.WithRemoteSession(cleanup, func(m *aistudio.SessionState) error {
			return aistudio.FinishRemoteRecoveryPending(cleanup, account.StoragePath, m.Remote, pendingID, submitted, remoteRecoveryCode(err), r.recoveryNow())
		})
		if finishErr != nil {
			authhelper.ObserveRecoveryStage(ctx, "control_blocked", finishErr, nil)
		}
	}
	if err != nil {
		err = safeRemoteRecoveryFailure(err)
		_ = lease.WithRemoteSession(cleanup, func(m *aistudio.SessionState) error {
			if m.Remote.Pending != nil {
				return aistudio.RecordRemoteRecoveryFailure(cleanup, account.StoragePath, m.Remote, remoteRecoveryCode(err), r.recoveryNow())
			}
			return nil
		})
		current, _ := lease.ReloadStorageState()
		m, _ := current.Session()
		authhelper.ObserveRecoveryStage(ctx, "commit_blocked", err, m)
		return true, err
	}
	// Deduplication is not itself proof that a still-pending recovery completed.
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
	if err := lease.CompleteRemoteRecovery(); err != nil {
		return true, err
	}
	authhelper.ObserveRecoveryStage(ctx, "commit", nil, latest)
	if prepared {
		controlErr := lease.WithRemoteSession(cleanup, func(m *aistudio.SessionState) error {
			return aistudio.CompleteRemoteRecoveryAuth(cleanup, account.StoragePath, m.Remote, r.recoveryNow())
		})
		if controlErr == nil {
			// Only this successful adoption may use the original, uncounted ACK.
			if ackErr := r.retryRemoteAck(ctx, lease, true); ackErr != nil {
				authhelper.ObserveRecoveryStage(ctx, "ack_pending", ackErr, latest)
				if aistudio.IsSessionFailureCode(ackErr, "IDENTITY_MISMATCH") || aistudio.IsSessionFailureCode(ackErr, "VERSION_CONFLICT") {
					return true, ackErr
				}
			}
		} else {
			authhelper.ObserveRecoveryStage(ctx, "control_blocked", controlErr, latest)
		}
	}
	return true, nil
}

func (r *authRuntimeRefresher) observeRemoteRecovery(ctx context.Context, account *aistudio.Account) context.Context {
	return authhelper.WithRecoveryObserver(ctx, func(v authhelper.RecoveryProgress) {
		if r.requests == nil {
			return
		}
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
	})
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
