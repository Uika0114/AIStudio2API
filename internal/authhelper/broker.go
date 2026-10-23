package authhelper

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type Mapping struct {
	Handle        string   `json:"handle"`
	Consumer      string   `json:"consumer"`
	GaiaID        string   `json:"gaia_id"`
	Email         string   `json:"email"`
	StateFile     string   `json:"state_file"`
	ProxyFile     string   `json:"proxy_file,omitempty"`
	DirectAuth    bool     `json:"direct_auth"`
	CookieDomains []string `json:"cookie_domains,omitempty"`
}

// RenewFunc is in-process dependency injection; no RPC can select an implementation.
type RenewFunc func(context.Context, aistudio.StorageState, Mapping, bool) (aistudio.StorageState, error)

type Broker struct {
	mappings  map[string]Mapping
	renew     RenewFunc
	lifecycle context.Context
	now       func() time.Time
	write     func(context.Context, string, *aistudio.StorageState) error
}

func NewBroker(ctx context.Context, mappings []Mapping, renew RenewFunc) (*Broker, error) {
	if ctx == nil || renew == nil {
		return nil, failure("INVALID_REQUEST")
	}
	b := &Broker{mappings: make(map[string]Mapping), renew: renew, lifecycle: ctx, now: time.Now, write: aistudio.WriteManagedSessionState}
	paths, identities := make(map[string]bool), make(map[string]bool)
	for _, m := range mappings {
		meta := aistudio.SessionState{Version: Protocol, Authority: &aistudio.SessionAuthority{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1}}
		if meta.Validate() != nil || !filepath.IsAbs(m.StateFile) || (m.ProxyFile != "" && !filepath.IsAbs(m.ProxyFile)) || (m.ProxyFile == "") == !m.DirectAuth {
			return nil, failure("INVALID_REQUEST")
		}
		path := strings.ToLower(filepath.Clean(m.StateFile))
		if b.mappings[m.Handle].Handle != "" || paths[path] || identities[m.GaiaID] {
			return nil, failure("IDENTITY_MISMATCH")
		}
		for _, d := range m.CookieDomains {
			if d == "" || strings.ContainsAny(d, "/\\ :\r\n") {
				return nil, failure("INVALID_REQUEST")
			}
		}
		paths[path], identities[m.GaiaID] = true, true
		b.mappings[m.Handle] = m
	}
	return b, nil
}

func (b *Broker) mapping(consumer, handle string) (Mapping, error) {
	m, ok := b.mappings[handle]
	if !ok || consumer != m.Consumer {
		return Mapping{}, failure("IDENTITY_MISMATCH")
	}
	return m, nil
}

func authority(state aistudio.StorageState, m Mapping, epoch uint64) (*aistudio.SessionState, error) {
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Authority == nil {
		return nil, failure("MAPPING_DISABLED")
	}
	a := meta.Authority
	if !a.Enabled {
		return nil, failure("MAPPING_DISABLED")
	}
	if a.Handle != m.Handle || a.Consumer != m.Consumer || a.GaiaID != m.GaiaID || a.Email != m.Email || a.Epoch != epoch {
		return nil, failure("IDENTITY_MISMATCH")
	}
	ext, exists, err := state.AuthExtension()
	if err != nil || !exists || ext.OAuth == nil || ext.OAuth.GaiaID != m.GaiaID || ext.Source.Email != m.Email || ext.Source.GaiaID != "" && ext.Source.GaiaID != m.GaiaID {
		return nil, failure("IDENTITY_MISMATCH")
	}
	return meta, nil
}

func (b *Broker) Refresh(ctx context.Context, consumer string, r RefreshRequest) (*RefreshResponse, error) {
	if !validRequest(r) {
		return nil, failure("INVALID_REQUEST")
	}
	m, err := b.mapping(consumer, r.Handle)
	if err != nil {
		return nil, err
	}
	release, err := aistudio.AcquireSessionAccountLease(ctx, m.StateFile)
	if err != nil {
		return nil, failure("BUSY")
	}
	defer release()
	state, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		return nil, failure("STATE_UNAVAILABLE")
	}
	meta, err := authority(state, m, r.Epoch)
	if err != nil {
		return nil, err
	}
	a := meta.Authority
	if result, historyErr, found := b.historyResult(state, meta, m, r); found {
		return result, historyErr
	}
	if o := a.Operation; o != nil && o.Epoch == a.Epoch {
		if o.ID == r.OperationID {
			if !operationMatches(o, r) {
				return nil, failure("OPERATION_CONFLICT")
			}
			switch o.Stage {
			case "ready", "acked":
				return resultFromState(state, meta, m, r)
			case "failed":
				return nil, operationFailure(o, r)
			default:
				return nil, failure("INDETERMINATE")
			}
		}
		if o.Stage == "ready" {
			return nil, failure("ACK_REQUIRED")
		}
		if o.Stage == "pending" || o.Stage == "indeterminate" {
			return nil, failure("INDETERMINATE")
		}
	}
	if a.CookieRevision != r.BaseCookie || a.MaterialVersion != r.BaseMaterial || a.CookieRevision == math.MaxUint64 || a.MaterialVersion == math.MaxUint64 {
		return nil, failure("VERSION_CONFLICT")
	}
	if r.Reason == "bootstrap" && a.Operation != nil && a.Operation.Epoch == a.Epoch && a.Operation.Failure != "NOT_STARTED" {
		return nil, failure("INVALID_REQUEST")
	}
	if r.LookupOnly {
		return nil, b.expiredUnstarted(state, meta, m, r)
	}
	now := b.now()
	if !requestTime(r).After(now) {
		return nil, b.expiredUnstarted(state, meta, m, r)
	}
	if requestTime(r).After(now.Add(2 * time.Minute)) {
		return nil, failure("INVALID_REQUEST")
	}
	// The server owns the operation after recording it. A disconnected HTTP client
	// cannot discard a successful result or release a still-running CNG operation.
	work, cancel := context.WithDeadline(b.lifecycle, requestTime(r))
	defer cancel()
	if a.Operation != nil {
		a.History = append(a.History, *a.Operation)
	}
	a.Operation = &aistudio.SessionOperation{ID: r.OperationID, Reason: r.Reason, Epoch: r.Epoch, BaseCookie: r.BaseCookie, BaseMaterial: r.BaseMaterial, Deadline: r.Deadline, Stage: "pending"}
	if state.SetSession(meta) != nil || b.write(work, m.StateFile, &state) != nil {
		return nil, failure("STATE_UNAVAILABLE")
	}
	meta, _ = state.Session()
	a = meta.Authority
	originalExt, _, _ := state.AuthExtension()
	work, httpCounts := countAuthentication(work)
	result := state
	if r.Reason == "reauth" {
		result, err = cloneState(state)
		if err == nil {
			result, err = b.renew(work, result, m, a.AllowSourceRecovery)
		}
	}
	a.Operation.HTTP = httpCounts.snapshot()
	if err == nil {
		err = ValidateCookies(result.Cookies, m.CookieDomains)
	}
	var materialChanged bool
	if err == nil {
		nextExt, exists, nextErr := result.AuthExtension()
		if nextErr != nil || !exists || nextExt.OAuth == nil || nextExt.OAuth.GaiaID != m.GaiaID || nextExt.Source.Email != m.Email || nextExt.Source != originalExt.Source {
			err = failure("IDENTITY_MISMATCH")
		} else {
			materialChanged = !reflect.DeepEqual(originalExt.OAuth, nextExt.OAuth)
			state.Cookies = result.Cookies
			if err = state.SetAuthExtension(nextExt); err == nil {
				a.CookieRevision++
				if materialChanged {
					a.MaterialVersion++
				}
				a.Operation.Stage = "ready"
				a.Operation.ResultCookie, a.Operation.ResultMaterial = a.CookieRevision, a.MaterialVersion
			}
		}
	}
	// Always try to persist the known result even when the caller or work deadline
	// has expired. This bounded save never issues another authentication request.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(b.lifecycle), 5*time.Second)
	defer saveCancel()
	if err != nil {
		code := safeCode(err)
		a.Operation.Stage, a.Operation.Failure = "indeterminate", "INDETERMINATE"
		switch code {
		case "MATERIAL_REJECTED", "SOURCE_REBIND_REQUIRED", "UNSUPPORTED_KEY":
			a.Operation.Stage, a.Operation.Failure = "failed", code
		}
		// Do not retain partial cookies/material from a failed backend.
		stored, loadErr := aistudio.LoadStorageState(m.StateFile)
		if loadErr != nil {
			return nil, failure("INDETERMINATE")
		}
		if stored.SetSession(meta) != nil || b.write(saveCtx, m.StateFile, &stored) != nil {
			return nil, failure("INDETERMINATE")
		}
		return nil, failure(a.Operation.Failure)
	}
	if state.SetSession(meta) != nil || b.write(saveCtx, m.StateFile, &state) != nil {
		return nil, failure("INDETERMINATE")
	}
	meta, _ = state.Session()
	return resultFromState(state, meta, m, r)
}

func resultFromState(state aistudio.StorageState, meta *aistudio.SessionState, m Mapping, r RefreshRequest) (*RefreshResponse, error) {
	a := meta.Authority
	if a.Operation == nil || a.Operation.ResultCookie != a.CookieRevision || a.Operation.ResultMaterial != a.MaterialVersion {
		return nil, failure("VERSION_CONFLICT")
	}
	if err := ValidateCookies(state.Cookies, m.CookieDomains); err != nil {
		return nil, err
	}
	return &RefreshResponse{Version: Protocol, Handle: r.Handle, Epoch: r.Epoch, OperationID: r.OperationID, BaseCookie: r.BaseCookie, BaseMaterial: r.BaseMaterial, CookieRevision: a.CookieRevision, MaterialVersion: a.MaterialVersion, GaiaID: a.GaiaID, Email: a.Email, Cookies: append([]aistudio.StateCookie(nil), state.Cookies...)}, nil
}

func (b *Broker) Ack(ctx context.Context, consumer string, r AckRequest) error {
	if !validAck(r) {
		return failure("INVALID_REQUEST")
	}
	m, err := b.mapping(consumer, r.Handle)
	if err != nil {
		return err
	}
	release, err := aistudio.AcquireSessionAccountLease(ctx, m.StateFile)
	if err != nil {
		return failure("BUSY")
	}
	defer release()
	state, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	meta, err := authority(state, m, r.Epoch)
	if err != nil {
		return err
	}
	a := meta.Authority
	if r.Mode != "" && r.Mode != "keep_current" {
		return failure("INVALID_REQUEST")
	}
	o := a.Operation
	if o == nil || o.ID != r.OperationID || (o.Stage != "ready" && o.Stage != "acked") || o.ResultCookie != r.CookieRevision || o.ResultMaterial != r.MaterialVersion || a.CookieRevision != r.CookieRevision || a.MaterialVersion != r.MaterialVersion {
		return failure("VERSION_CONFLICT")
	}
	if o.Stage == "acked" {
		if o.AckMode != r.Mode {
			return failure("OPERATION_CONFLICT")
		}
		return nil
	}
	o.Stage = "acked"
	o.AckMode = r.Mode
	if state.SetSession(meta) != nil || b.write(ctx, m.StateFile, &state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return nil
}

func cloneState(s aistudio.StorageState) (aistudio.StorageState, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return aistudio.StorageState{}, failure("INVALID_REQUEST")
	}
	var result aistudio.StorageState
	if json.Unmarshal(data, &result) != nil {
		return result, failure("INVALID_REQUEST")
	}
	return result, nil
}
