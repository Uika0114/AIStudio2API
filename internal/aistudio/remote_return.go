package aistudio

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"time"

	"github.com/gofrs/flock"
)

const retiredAuthorityKey = "aistudio2api_retired_authority"

// RetiredAuthority is metadata only, used to keep epochs monotonic after a
// controlled return to local ownership. It contains no OAuth or cookies.
func (state StorageState) RetiredAuthority() (*SessionAuthority, error) {
	raw, exists := state.extra[retiredAuthorityKey]
	if !exists {
		return nil, nil
	}
	var value SessionAuthority
	if json.Unmarshal(raw, &value) != nil || value.Epoch == 0 {
		return nil, SessionError("INVALID_SESSION_STATE")
	}
	return &value, nil
}

// ReturnSessionAuthorityToLocal is deliberately separate from generic writers.
// Caller must hold the account lease and explicitly confirm the old consumer is stopped.
func ReturnSessionAuthorityToLocal(ctx context.Context, path string, state *StorageState) error {
	target, err := filepath.Abs(path)
	if err != nil {
		return SessionError("INVALID_STATE_PATH")
	}
	lock := flock.New(target + ".session.lock")
	ok, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil || !ok {
		return SessionError("BUSY")
	}
	defer lock.Unlock()
	persisted, err := LoadStorageState(target)
	if err != nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	old, err := persisted.Session()
	next, nextErr := state.Session()
	if err != nil || nextErr != nil || old == nil || old.Authority == nil || !reflect.DeepEqual(old, next) {
		return SessionError("VERSION_CONFLICT")
	}
	authority := old.Authority
	if authority.Enabled || authority.Epoch == math.MaxUint64 {
		return SessionError("MAPPING_DISABLED")
	}
	if op := authority.Operation; op != nil && (op.Stage == "pending" || op.Stage == "indeterminate") {
		return SessionError("INDETERMINATE")
	}
	authority.Epoch++
	archive, err := json.Marshal(authority)
	if err != nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	value := *state
	value.extra = make(map[string]json.RawMessage, len(state.extra))
	for key, raw := range state.extra {
		value.extra[key] = raw
	}
	value.extra[retiredAuthorityKey] = archive
	delete(value.extra, sessionExtensionKey)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	if atomicWriteFile(target, append(data, '\n'), 0o600) != nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	*state = value
	return nil
}
