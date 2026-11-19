package aistudio

import (
	"context"
	"strings"
	"time"
)

// Purposes are fixed internally. Resource affinity alone never grants result-only
// access, and a result lease cannot be reused to generate or renew authentication.
type accountLeasePurpose uint8

const (
	leaseGeneration accountLeasePurpose = iota
	leaseManagement
	leaseRemoteMaintenance
	leaseResourceResult
)

func remoteQuarantineCode(state StorageState) string {
	meta, err := state.Session()
	if err != nil {
		return "INVALID_SESSION_STATE"
	}
	if meta == nil {
		return ""
	}
	if meta.Authority != nil {
		return "REMOTE_OWNED"
	}
	switch {
	case meta.Remote.EnrollmentRequired:
		return "BOOTSTRAP_REQUIRED"
	case meta.Remote.Pending != nil:
		return "PENDING_RECOVERY"
	case meta.Remote.RecoveryRequired:
		return "RECOVERY_REQUIRED"
	default:
		// An ACK is transport compensation for an already committed projection.
		return ""
	}
}

func (p *AccountPool) publishRemoteQuarantineLocked(account *Account) {
	code := remoteQuarantineCode(account.StorageState)
	if code == "" {
		return
	}
	if !account.Config.Enabled {
		account.State = AccountDisabled
	} else if account.authRefreshers == 0 {
		account.State = AccountAuthRequired
		if code == "REMOTE_OWNED" || code == "INVALID_SESSION_STATE" {
			account.State = AccountUnavailable
		}
	}
	account.stateMessage = code
}

func generationCandidate(account *Account) bool {
	if account == nil || !account.Config.Enabled || account.verificationIDLocked() != "" {
		return false
	}
	// A live refresh is busy, not a failed authentication. It remains waitable,
	// but the acquisition path cannot grant a slot until it has finished.
	if account.authRefreshers > 0 {
		return account.State == AccountReady || account.State == AccountAuthRequired
	}
	return account.State == AccountReady && remoteQuarantineCode(account.StorageState) == ""
}

func resourceResultCandidate(account *Account) bool {
	if account == nil || !account.Config.Enabled || account.verificationIDLocked() != "" {
		return false
	}
	code := remoteQuarantineCode(account.StorageState)
	if code == "BOOTSTRAP_REQUIRED" || code == "REMOTE_OWNED" || code == "INVALID_SESSION_STATE" {
		return false
	}
	if account.State == AccountReady {
		return true
	}
	meta, err := account.StorageState.Session()
	return account.State == AccountAuthRequired && err == nil && meta != nil && meta.Remote != nil
}

// AcquireRemoteMaintenance obtains the ordinary exclusive account/file lock for
// an explicitly selected authentication maintenance action, not generation.
func (p *AccountPool) AcquireRemoteMaintenance(ctx context.Context, accountID string) (*AccountLease, error) {
	return p.acquireAccount(ctx, accountID, leaseRemoteMaintenance)
}

// RemoteAuthenticationAllowed distinguishes explicit maintenance from passive
// management and bound resource reads. It never bypasses a file or identity lock.
func (l *AccountLease) RemoteAuthenticationAllowed() bool {
	if l == nil {
		return false
	}
	if l.purpose == leaseGeneration || l.purpose == leaseRemoteMaintenance {
		return true
	}
	if l.purpose == leaseManagement && l.pool != nil && l.account != nil {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		meta, err := l.account.StorageState.Session()
		return err == nil && meta == nil
	}
	return false
}

// RefreshRemoteQuarantine rereads the authoritative session while the caller
// still holds its lease. It only publishes blockers; it never declares recovery.
func (l *AccountLease) RefreshRemoteQuarantine() error {
	_, err := l.ReloadStorageState()
	return err
}

// CompleteRemoteRecovery is called only after projection commit and runtime
// takeover. It checks the lease's current generation and disk CAS before clearing
// a fault; resource successes and cached catalogs cannot call this implicitly.
func (l *AccountLease) CompleteRemoteRecovery() error {
	if l == nil || l.pool == nil || l.account == nil || !l.RemoteAuthenticationAllowed() {
		return SessionError("RECOVERY_NOT_ALLOWED")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released {
		return SessionError("CANCELED")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.pool.byID[l.account.ID] != l.account || l.authGeneration != l.account.authGeneration {
		return SessionError("VERSION_CONFLICT")
	}
	meta, err := l.account.StorageState.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	if code := remoteQuarantineCode(l.account.StorageState); code != "" {
		return SessionError(code)
	}
	if err := CheckSessionRevision(l.account.StoragePath, l.account.StorageState); err != nil {
		return err
	}
	if !l.account.Config.Enabled {
		l.account.State = AccountDisabled
	} else {
		l.account.State = AccountReady
	}
	l.account.stateMessage = ""
	l.account.authCheckedAt = time.Now().UTC()
	l.pool.notifyLocked()
	return nil
}

func resourceSelection(resourceID string) AccountSelection {
	return AccountSelection{ResourceID: strings.TrimSpace(resourceID), purpose: leaseResourceResult}
}

func leasePurposeAllowsSelection(lease *AccountLease, selection AccountSelection) bool {
	if lease.purpose != leaseResourceResult {
		// Management/maintenance may read catalogs, but may not become a model
		// generation lease simply by carrying it in a context.
		return lease.purpose == leaseGeneration || selection.ModelID == "" && selection.Method == ""
	}
	if selection.purpose != leaseResourceResult {
		return false
	}
	return selection.ResourceID != "" && selection.ResourceID == lease.resourceID
}

type remoteAuthenticationDisabledKey struct{}

func withoutRemoteAuthentication(ctx context.Context) context.Context {
	return context.WithValue(ctx, remoteAuthenticationDisabledKey{}, true)
}

// RemoteAuthenticationAllowed rejects automatic authentication during bound
// resource operations, including when they reuse an existing generation lease.
func RemoteAuthenticationAllowed(ctx context.Context) bool {
	if disabled, _ := ctx.Value(remoteAuthenticationDisabledKey{}).(bool); disabled {
		return false
	}
	lease, ok := AccountLeaseFromContext(ctx)
	return ok && lease.RemoteAuthenticationAllowed()
}
