package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const SessionProtocol = 1
const sessionExtensionKey = "aistudio2api_session"

// SessionFailure never exposes an underlying transport, credential or file error.
// Callers must recognize this marker before generic network/RPC retry classifiers.
type SessionFailure struct{ Code string }

func (e *SessionFailure) Error() string             { return "remote authentication: " + e.Code }
func (e *SessionFailure) StopGenerationRetry() bool { return true }
func SessionError(code string) error                { return &SessionFailure{Code: code} }
func IsSessionFailure(err error) bool               { var failure *SessionFailure; return errors.As(err, &failure) }
func IsSessionFailureCode(err error, code string) bool {
	var failure *SessionFailure
	return errors.As(err, &failure) && failure.Code == code
}

// SessionOperation contains metadata only. It must never contain a response body.
type AuthHTTPCounts struct {
	Sent      int `json:"sent"`
	Responses int `json:"responses"`
}

type SessionOperation struct {
	ID             string          `json:"id"`
	Reason         string          `json:"reason"`
	Epoch          uint64          `json:"epoch"`
	BaseCookie     uint64          `json:"base_cookie"`
	BaseMaterial   uint64          `json:"base_material"`
	StateRevision  uint64          `json:"state_revision,omitempty"`
	Deadline       int64           `json:"deadline"`
	Stage          string          `json:"stage,omitempty"`
	ResultCookie   uint64          `json:"result_cookie,omitempty"`
	ResultMaterial uint64          `json:"result_material,omitempty"`
	Failure        string          `json:"failure,omitempty"`
	HTTP           *AuthHTTPCounts `json:"http,omitempty"`
	AckMode        string          `json:"ack_mode,omitempty"`
}

type SessionAck struct {
	ID       string `json:"id"`
	Epoch    uint64 `json:"epoch"`
	Cookie   uint64 `json:"cookie"`
	Material uint64 `json:"material"`
	Mode     string `json:"mode,omitempty"`
}

type RemoteSession struct {
	Handle             string                 `json:"handle"`
	Consumer           string                 `json:"consumer"`
	GaiaID             string                 `json:"gaia_id"`
	Email              string                 `json:"email"`
	Epoch              uint64                 `json:"epoch"`
	CookieRevision     uint64                 `json:"cookie_revision"`
	MaterialVersion    uint64                 `json:"material_version"`
	Pending            *SessionOperation      `json:"pending,omitempty"`
	Ack                *SessionAck            `json:"ack,omitempty"`
	EnrollmentRequired bool                   `json:"enrollment_required,omitempty"`
	RecoveryRequired   bool                   `json:"recovery_required,omitempty"`
	RetiredOperations  []SessionOperation     `json:"retired_operations,omitempty"`
	Retired            []RetiredRemoteBinding `json:"retired,omitempty"`
}

type SessionAuthority struct {
	Handle              string             `json:"handle"`
	Consumer            string             `json:"consumer"`
	GaiaID              string             `json:"gaia_id"`
	Email               string             `json:"email"`
	Epoch               uint64             `json:"epoch"`
	CookieRevision      uint64             `json:"cookie_revision"`
	MaterialVersion     uint64             `json:"material_version"`
	Enabled             bool               `json:"enabled"`
	AllowSourceRecovery bool               `json:"allow_source_recovery"`
	WindowsUserSID      string             `json:"windows_user_sid,omitempty"`
	Operation           *SessionOperation  `json:"operation,omitempty"`
	History             []SessionOperation `json:"history,omitempty"`
}

// SessionState is stored in the same atomic document as cookies and OAuth.
// Revision covers every write, not just an authentication generation in RAM.
type SessionState struct {
	Version   int               `json:"version"`
	Revision  uint64            `json:"revision"`
	Remote    *RemoteSession    `json:"remote,omitempty"`
	Authority *SessionAuthority `json:"authority,omitempty"`
}

func (s StorageState) Session() (*SessionState, error) {
	raw, exists := s.extra[sessionExtensionKey]
	if !exists {
		return nil, nil
	}
	var result SessionState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, SessionError("INVALID_SESSION_STATE")
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	if result.Remote != nil {
		extension, exists, err := s.AuthExtension()
		if err != nil || exists && (extension.OAuth != nil || extension.Source.ChromeRoot != "" || extension.Source.Profile != "") {
			return nil, SessionError("LOCAL_MATERIAL_ON_REMOTE")
		}
	}
	return &result, nil
}

func (s *StorageState) SetSession(value *SessionState) error {
	if s == nil || value == nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	if err := value.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	if s.extra == nil {
		s.extra = make(map[string]json.RawMessage)
	}
	s.extra[sessionExtensionKey] = raw
	return nil
}

func validSessionIdentity(handle, consumer, gaia, email string, epoch, material uint64) bool {
	return len(handle) >= 16 && len(handle) <= 128 && simpleSessionID(handle) &&
		len(consumer) > 0 && len(consumer) <= 200 && !strings.ContainsAny(consumer, "\r\n /\\") &&
		len(gaia) > 0 && len(gaia) <= 100 && simpleSessionID(gaia) &&
		len(email) <= 320 && strings.Contains(email, "@") && email == strings.ToLower(strings.TrimSpace(email)) &&
		!strings.ContainsAny(email, "\r\n\t ") && epoch > 0 && material > 0
}
func simpleSessionID(value string) bool {
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return value != ""
}
func ValidSessionOperationID(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && simpleSessionID(value)
}

func (s SessionState) Validate() error {
	if s.Version != SessionProtocol || (s.Remote == nil) == (s.Authority == nil) {
		return SessionError("INVALID_SESSION_STATE")
	}
	if r := s.Remote; r != nil {
		if !validSessionIdentity(r.Handle, r.Consumer, r.GaiaID, r.Email, r.Epoch, r.MaterialVersion) {
			return SessionError("INVALID_SESSION_STATE")
		}
		if r.Pending != nil && r.Ack != nil {
			return SessionError("INVALID_SESSION_STATE")
		}
		if r.Pending != nil && (!validOperation(r.Pending) || r.Pending.Epoch != r.Epoch || r.Pending.BaseCookie != r.CookieRevision || r.Pending.BaseMaterial != r.MaterialVersion) {
			return SessionError("INVALID_SESSION_STATE")
		}
		if r.Ack != nil && (!ValidSessionOperationID(r.Ack.ID) || r.Ack.Epoch != r.Epoch || r.Ack.Cookie != r.CookieRevision || r.Ack.Material != r.MaterialVersion) {
			return SessionError("INVALID_SESSION_STATE")
		}
	}
	if a := s.Authority; a != nil {
		if !validSessionIdentity(a.Handle, a.Consumer, a.GaiaID, a.Email, a.Epoch, a.MaterialVersion) {
			return SessionError("INVALID_SESSION_STATE")
		}
		if a.Operation != nil && !validOperation(a.Operation) {
			return SessionError("INVALID_SESSION_STATE")
		}
	}
	return nil
}
func validOperation(o *SessionOperation) bool {
	if !ValidSessionOperationID(o.ID) || o.Epoch == 0 || o.BaseMaterial == 0 || (o.Reason != "bootstrap" && o.Reason != "reauth") || o.Deadline <= 0 {
		return false
	}
	switch o.Stage {
	case "", "pending", "ready", "acked", "failed", "indeterminate":
		return true
	}
	return false
}

// AcquireSessionAccountLease shares the application's existing account file lease.
// The helper never loads an AccountPool and never holds just an in-process mutex.
func AcquireSessionAccountLease(ctx context.Context, storagePath string) (func(), error) {
	for {
		if ctx.Err() != nil {
			return nil, SessionError("BUSY")
		}
		lock, _, err := acquireAccountFileLeaseFor(storagePath, true)
		if err == nil {
			if lock == nil {
				return nil, SessionError("INVALID_STATE_PATH")
			}
			return func() { _ = lock.Unlock() }, nil
		}
		if !errors.Is(err, errAccountLeaseBusy) {
			return nil, SessionError("STATE_UNAVAILABLE")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, SessionError("BUSY")
		case <-timer.C:
		}
	}
}

// WriteManagedSessionState is reserved for the native provider/registration/broker.
// Ordinary cookie writers cannot change bindings or modify an authority document.
func WriteManagedSessionState(ctx context.Context, path string, state *StorageState) error {
	return writeSessionState(ctx, path, state, true)
}

func writeSessionState(ctx context.Context, path string, state *StorageState, managed bool) error {
	if state == nil || ctx.Err() != nil {
		return SessionError("CANCELED")
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return SessionError("INVALID_STATE_PATH")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return SessionError("STATE_UNAVAILABLE")
	}
	lock := flock.New(target + ".session.lock")
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !locked {
		return SessionError("STATE_BUSY")
	}
	defer lock.Unlock()
	if err := state.Validate(); err != nil {
		return err
	}
	next, err := state.Session()
	if err != nil {
		return err
	}
	var old *SessionState
	previous, readErr := LoadStorageState(target)
	if readErr == nil {
		old, err = previous.Session()
		if err != nil {
			return err
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if !managed && readErr == nil && reflect.DeepEqual(previous, *state) {
		return nil
	}
	if old != nil {
		if next == nil || old.Revision != next.Revision {
			return SessionError("VERSION_CONFLICT")
		}
		if old.Authority != nil && !managed {
			return SessionError("REMOTE_OWNED")
		}
		if !managed && !reflect.DeepEqual(old.Remote, next.Remote) {
			return SessionError("BINDING_PROTECTED")
		}
		if (old.Remote == nil) != (next.Remote == nil) {
			return SessionError("BINDING_PROTECTED")
		}
		if old.Remote != nil && !sameRemoteIdentity(*old.Remote, *next.Remote) {
			if !managed || !next.Remote.EnrollmentRequired || next.Remote.Epoch <= old.Remote.Epoch || next.Remote.GaiaID != old.Remote.GaiaID || next.Remote.Email != old.Remote.Email || next.Remote.CookieRevision < old.Remote.CookieRevision || next.Remote.MaterialVersion < old.Remote.MaterialVersion {
				return SessionError("IDENTITY_MISMATCH")
			}
		}
		if old.Authority != nil && (old.Authority.GaiaID != next.Authority.GaiaID || old.Authority.Email != next.Authority.Email || old.Authority.WindowsUserSID != next.Authority.WindowsUserSID || next.Authority.Epoch < old.Authority.Epoch) {
			return SessionError("IDENTITY_MISMATCH")
		}
	} else if next != nil && (!managed || next.Revision != 0) {
		return SessionError("BINDING_PROTECTED")
	}
	value := *state
	// Make revision changes local until atomic replacement succeeds.
	if next != nil {
		if next.Revision == math.MaxUint64 {
			return SessionError("VERSION_CONFLICT")
		}
		value.extra = make(map[string]json.RawMessage, len(state.extra))
		for k, v := range state.extra {
			value.extra[k] = v
		}
		next.Revision++
		if err := value.SetSession(next); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return SessionError("INVALID_SESSION_STATE")
	}
	if err := atomicWriteFile(target, append(data, '\n'), 0o600); err != nil {
		return err
	}
	*state = value
	return nil
}

func sameRemoteIdentity(a, b RemoteSession) bool {
	return a.Handle == b.Handle && a.Consumer == b.Consumer && a.GaiaID == b.GaiaID && a.Email == b.Email && a.Epoch == b.Epoch
}

// SessionAuthorityOwned checks the on-disk fence after taking the account lease.
func SessionAuthorityOwned(path string) (bool, error) {
	state, err := LoadStorageState(path)
	if err != nil {
		return false, err
	}
	meta, err := state.Session()
	return meta != nil && meta.Authority != nil, err
}
