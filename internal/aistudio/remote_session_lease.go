package aistudio

import "context"

// CheckSessionRevision is checked before resetting a runtime, then again at save.
func CheckSessionRevision(path string, state StorageState) error {
	expected, err := state.Session()
	if err != nil || expected == nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	current, err := LoadStorageState(path)
	if err != nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	actual, err := current.Session()
	if err != nil || actual == nil || actual.Revision != expected.Revision {
		return SessionError("VERSION_CONFLICT")
	}
	return nil
}

// UpdateRemoteSession only changes metadata, never cookies, origins, account config
// or authGeneration. It is used for durable pending/ack reconciliation.
func (l *AccountLease) UpdateRemoteSession(ctx context.Context, update func(*SessionState) error) error {
	if l == nil || l.pool == nil || l.account == nil || update == nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	l.operation.Lock()
	defer l.operation.Unlock()
	if l.released || ctx.Err() != nil {
		return SessionError("CANCELED")
	}
	l.account.storageMu.Lock()
	defer l.account.storageMu.Unlock()
	state, err := LoadStorageState(l.account.StoragePath)
	if err != nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	revision := meta.Revision
	if err := update(meta); err != nil {
		return err
	}
	if meta.Revision != revision {
		return SessionError("VERSION_CONFLICT")
	}
	if err := state.SetSession(meta); err != nil {
		return err
	}
	if err := WriteManagedSessionState(ctx, l.account.StoragePath, &state); err != nil {
		return err
	}
	l.pool.mu.Lock()
	l.account.StorageState = state
	l.pool.mu.Unlock()
	return nil
}
