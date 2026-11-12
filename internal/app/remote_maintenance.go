package app

import (
	"context"
	"errors"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

type remoteMaintenanceKey struct{}

func (r *authRuntimeRefresher) recoveryNow() time.Time {
	if r.recoveryClock != nil {
		return r.recoveryClock()
	}
	return time.Now()
}

func remoteRecoveryCode(err error) string {
	if err == nil {
		return "OK"
	}
	var failure *aistudio.SessionFailure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return "STATE_UNAVAILABLE"
}

// Only registered episodes are revisited. Startup reads their persisted control
// records once; it never grants a budget to historical pending operations.
func (r *authRuntimeRefresher) startRemoteMaintenance(parent context.Context, accounts []*aistudio.Account) func() {
	if r.remote == nil || r.pool == nil {
		return func() {}
	}
	for _, account := range accounts {
		meta, err := account.StorageState.Session()
		if err != nil || meta == nil || meta.Remote == nil {
			continue
		}
		status, err := aistudio.ReadRemoteRecoveryStatus(account.StoragePath, meta.Remote, true, r.recoveryNow())
		if err == nil && status.Action != "none" {
			r.registerRemoteRecovery(account.ID)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runRemoteMaintenance(ctx)
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (r *authRuntimeRefresher) registerRemoteRecovery(id string) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()
	if r.recoveryTasks == nil {
		r.recoveryTasks = make(map[string]struct{})
	}
	r.recoveryTasks[id] = struct{}{}
}

func (r *authRuntimeRefresher) forgetRemoteRecovery(id string) {
	r.recoveryMu.Lock()
	delete(r.recoveryTasks, id)
	r.recoveryMu.Unlock()
}

func (r *authRuntimeRefresher) runRemoteMaintenance(ctx context.Context) {
	r.recoveryMu.Lock()
	ids := make([]string, 0, len(r.recoveryTasks))
	for id := range r.recoveryTasks {
		ids = append(ids, id)
	}
	r.recoveryMu.Unlock()
	enabledIDs, _ := r.pool.EnabledAccounts()
	enabled := make(map[string]bool, len(enabledIDs))
	for _, id := range enabledIDs {
		enabled[id] = true
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		account, err := r.pool.Account(id)
		if err != nil {
			r.forgetRemoteRecovery(id)
			continue
		}
		// Read-only scheduling hints. The lease revalidates before any action.
		status := remoteRecoveryStatus(account.StoragePath, true, r.recoveryNow())
		if status.Action == "none" {
			r.forgetRemoteRecovery(id)
			continue
		}
		if !enabled[id] || status.Code != "OK" {
			continue
		}
		r.runRemoteMaintenanceAccount(ctx, id)
	}
}

func (r *authRuntimeRefresher) runRemoteMaintenanceAccount(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(parent, 100*time.Second)
	defer cancel()
	lease, err := r.pool.AcquireRemoteMaintenance(ctx, id)
	if err != nil {
		return
	}
	defer lease.Release()
	ctx = aistudio.ContextWithAccountLease(ctx, lease)
	ctx = context.WithValue(ctx, remoteMaintenanceKey{}, true)
	var status aistudio.RemoteRecoveryStatus
	err = lease.WithRemoteSession(ctx, func(m *aistudio.SessionState) error {
		var e error
		status, e = aistudio.ReadRemoteRecoveryStatus(lease.Account().StoragePath, m.Remote, lease.Account().Config.Enabled, r.recoveryNow())
		return e
	})
	if err != nil || status.Code != "OK" {
		return
	}
	if status.Action == "ack" {
		_ = r.retryRemoteAck(ctx, lease, false)
		_ = lease.RefreshRemoteQuarantine()
		r.publishRecoveryAccounts()
		return
	}
	if status.Action == "pending" || status.Action == "new_auth" {
		_, _ = r.tryRemoteRefresh(ctx)
	}
}

func remoteRecoveryStatus(path string, enabled bool, now time.Time) aistudio.RemoteRecoveryStatus {
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		return aistudio.RemoteRecoveryStatus{Action: "none", Code: "STATE_UNAVAILABLE"}
	}
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return aistudio.RemoteRecoveryStatus{Action: "none", Code: "INVALID_SESSION_STATE"}
	}
	status, _ := aistudio.ReadRemoteRecoveryStatus(path, meta.Remote, enabled, now)
	return status
}

func (r *authRuntimeRefresher) retryRemoteAck(ctx context.Context, lease *aistudio.AccountLease, initial bool) error {
	account := lease.Account()
	id, reserved := "", false
	err := lease.WithRemoteSession(ctx, func(m *aistudio.SessionState) error {
		if m.Remote.Ack == nil {
			return nil
		}
		if m.Remote.Email != account.ID || m.Remote.Consumer != r.remote.Consumer() {
			return aistudio.SessionError("IDENTITY_MISMATCH")
		}
		id = m.Remote.Ack.ID
		if !initial {
			if err := aistudio.ReserveRemoteRecoveryAck(ctx, account.StoragePath, m.Remote, r.recoveryNow()); err != nil {
				return err
			}
			reserved = true
		}
		return nil
	})
	if err != nil || id == "" {
		return err
	}
	key := account.ID + "/" + id
	err = lease.UpdateRemoteSession(ctx, func(m *aistudio.SessionState) error {
		if m.Remote.Ack == nil || m.Remote.Ack.ID != id {
			return aistudio.SessionError("VERSION_CONFLICT")
		}
		if _, accepted := r.ackAccepted.Load(key); !accepted {
			if err := r.remote.AckSession(ctx, m.Remote); err != nil {
				return err
			}
			r.ackAccepted.Store(key, true)
		}
		m.Remote.Ack = nil
		return nil
	})
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	controlErr := lease.WithRemoteSession(cleanup, func(m *aistudio.SessionState) error {
		if reserved {
			if e := aistudio.FinishRemoteRecoveryAck(cleanup, account.StoragePath, m.Remote, id, remoteRecoveryCode(err), r.recoveryNow()); e != nil {
				return e
			}
		}
		if err != nil {
			if initial {
				return aistudio.RecordRemoteRecoveryAckFailure(cleanup, account.StoragePath, m.Remote, remoteRecoveryCode(err), r.recoveryNow())
			}
			return nil
		}
		return aistudio.CompleteRemoteRecoveryAck(cleanup, account.StoragePath, m.Remote, id)
	})
	if err == nil {
		r.ackAccepted.Delete(key)
		return controlErr
	}
	// A transport-only ACK loss does not invalidate committed cookies. A
	// definitive identity/version conflict is new evidence requiring maintenance.
	if aistudio.IsSessionFailureCode(err, "IDENTITY_MISMATCH") || aistudio.IsSessionFailureCode(err, "VERSION_CONFLICT") {
		_ = lease.UpdateRemoteSession(cleanup, func(m *aistudio.SessionState) error {
			m.Remote.RecoveryRequired = true
			return nil
		})
	}
	r.registerRemoteRecovery(account.ID)
	return safeRemoteRecoveryFailure(err)
}

func (r *authRuntimeRefresher) publishRecoveryAccounts() {
	if r.requests == nil || r.pool == nil {
		return
	}
	statuses := r.pool.Status()
	accounts := make([]api.AdminAccount, 0, len(statuses))
	for _, status := range statuses {
		accounts = append(accounts, adminAccountDTO(status, r.pool))
	}
	r.requests.publish(api.AdminEvent{Type: "accounts", Data: map[string]any{"accounts": accounts}})
}
