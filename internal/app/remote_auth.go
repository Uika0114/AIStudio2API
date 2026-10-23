package app

import (
	"context"
	"errors"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/authhelper"
)

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
		return true, err
	}
	// Cookies have committed. A lost ack must not turn the successful recovery
	// into a new auth workflow or discard the valid session. Next recovery retries it.
	_ = lease.UpdateRemoteSession(ctx, func(s *aistudio.SessionState) error {
		if s.Remote.Ack == nil {
			return nil
		}
		if err := r.remote.AckSession(ctx, s.Remote); err != nil {
			return err
		}
		s.Remote.Ack = nil
		return nil
	})
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
