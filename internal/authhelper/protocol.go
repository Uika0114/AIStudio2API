// Package authhelper implements a narrow session projection protocol, not remote execution.
package authhelper

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

const Protocol = aistudio.SessionProtocol
const MaxBody = 2 << 20

type RefreshRequest struct {
	Version      int    `json:"version"`
	Handle       string `json:"handle"`
	Epoch        uint64 `json:"mapping_epoch"`
	OperationID  string `json:"operation_id"`
	BaseCookie   uint64 `json:"base_cookie_revision"`
	BaseMaterial uint64 `json:"base_material_version"`
	Reason       string `json:"reason"`
	Deadline     int64  `json:"deadline"`
	LookupOnly   bool   `json:"lookup_only,omitempty"`
}

type RefreshResponse struct {
	Version         int                    `json:"version"`
	Handle          string                 `json:"handle"`
	Epoch           uint64                 `json:"mapping_epoch"`
	OperationID     string                 `json:"operation_id"`
	BaseCookie      uint64                 `json:"base_cookie_revision"`
	BaseMaterial    uint64                 `json:"base_material_version"`
	CookieRevision  uint64                 `json:"cookie_revision"`
	MaterialVersion uint64                 `json:"material_version"`
	GaiaID          string                 `json:"gaia_id"`
	Email           string                 `json:"email"`
	Cookies         []aistudio.StateCookie `json:"cookies"`
}

type AckRequest struct {
	Version         int    `json:"version"`
	Handle          string `json:"handle"`
	Epoch           uint64 `json:"mapping_epoch"`
	OperationID     string `json:"operation_id"`
	CookieRevision  uint64 `json:"cookie_revision"`
	MaterialVersion uint64 `json:"material_version"`
	Mode            string `json:"mode,omitempty"`
}

type Result struct {
	Code       string           `json:"code"`
	Session    *RefreshResponse `json:"session,omitempty"`
	NotStarted *RefreshRequest  `json:"not_started,omitempty"`
}

func failure(code string) error { return aistudio.SessionError(code) }
func operationRequest(o *aistudio.SessionOperation, handle string) RefreshRequest {
	return RefreshRequest{Version: Protocol, Handle: handle, Epoch: o.Epoch, OperationID: o.ID, BaseCookie: o.BaseCookie, BaseMaterial: o.BaseMaterial, Reason: o.Reason, Deadline: o.Deadline}
}
func validRequest(r RefreshRequest) bool {
	return r.Version == Protocol && aistudio.ValidSessionOperationID(r.Handle) && aistudio.ValidSessionOperationID(r.OperationID) && r.Epoch > 0 && r.BaseMaterial > 0 && r.Deadline > 0 && (r.Reason == "bootstrap" || r.Reason == "reauth")
}
func validAck(r AckRequest) bool {
	return r.Version == Protocol && aistudio.ValidSessionOperationID(r.Handle) && aistudio.ValidSessionOperationID(r.OperationID) && r.Epoch > 0 && r.CookieRevision > 0 && r.MaterialVersion > 0
}
func operationMatches(o *aistudio.SessionOperation, r RefreshRequest) bool {
	return o.ID == r.OperationID && o.Epoch == r.Epoch && o.BaseCookie == r.BaseCookie && o.BaseMaterial == r.BaseMaterial && o.Reason == r.Reason && o.Deadline == r.Deadline
}

// cookieDomainAllowed shares the existing local policy without normalizing cookies.
func cookieDomainAllowed(domain string, domains []string) bool {
	if len(domains) == 0 {
		domains = []string{"google.com"}
	}
	domain = strings.ToLower(strings.TrimPrefix(domain, "."))
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(d, "."))
		if d != "" && (domain == d || strings.HasSuffix(domain, "."+d)) {
			return true
		}
	}
	return false
}

// projectCookies selects the remote subset without changing the complete authority.
// Selection preserves every field and its order; in-domain invalid entries still fail.
func projectCookies(cookies []aistudio.StateCookie, domains []string) ([]aistudio.StateCookie, error) {
	var projected []aistudio.StateCookie
	for _, c := range cookies {
		if cookieDomainAllowed(c.Domain, domains) {
			projected = append(projected, c)
		}
	}
	if err := ValidateCookies(projected, domains); err != nil {
		return nil, err
	}
	return projected, nil
}

// Domain policy is local on both ends; never populated from an RPC request.
// The conservative default deliberately fails on unknown country/partition forms.
func ValidateCookies(cookies []aistudio.StateCookie, domains []string) error {
	if len(cookies) == 0 || len(cookies) > 128 {
		return failure("COOKIE_POLICY_FAILURE")
	}
	seen := make(map[string]bool)
	for _, c := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		allowed := cookieDomainAllowed(c.Domain, domains)
		key := c.Name + "\x00" + domain + "\x00" + c.Path
		if !allowed || seen[key] || c.PartitionKey != "" || c.Name == "" || len(c.Name) > 256 || len(c.Value) > 16384 || strings.ContainsAny(c.Name, "\r\n\t ;,=\"\\") || strings.ContainsAny(c.Value, "\r\n\x00") || math.IsNaN(c.Expires) || math.IsInf(c.Expires, 0) {
			return failure("COOKIE_POLICY_FAILURE")
		}
		seen[key] = true
	}
	state := aistudio.StorageState{Cookies: cookies}
	if state.Validate() != nil {
		return failure("COOKIE_POLICY_FAILURE")
	}
	if _, err := aistudio.NewSigner().Sign(state); err != nil {
		return failure("COOKIE_POLICY_FAILURE")
	}
	return nil
}

func safeCode(err error) string {
	var e *aistudio.SessionFailure
	if errors.As(err, &e) {
		switch e.Code {
		case "HELPER_UNAVAILABLE", "BUSY", "MAPPING_DISABLED", "IDENTITY_MISMATCH", "VERSION_CONFLICT", "SOURCE_REBIND_REQUIRED", "MATERIAL_REJECTED", "UNSUPPORTED_KEY", "AUTH_NETWORK_FAILURE", "INDETERMINATE", "COOKIE_POLICY_FAILURE", "INVALID_REQUEST", "ACK_REQUIRED", "STATE_UNAVAILABLE", "CANCELED", "OPERATION_CONFLICT", "REMOTE_OWNED":
			return e.Code
		case "REMOTE_DISABLED", "BOOTSTRAP_REQUIRED", "RECOVERY_BUDGET_REQUIRED", "RECOVERY_BUDGET_EXHAUSTED":
			return e.Code
		case "NOT_STARTED", "RESULT_SUPERSEDED":
			return e.Code
		}
	}
	return "INDETERMINATE"
}

func statusForError(err error) int {
	switch safeCode(err) {
	case "BUSY", "HELPER_UNAVAILABLE":
		return http.StatusServiceUnavailable
	default:
		return http.StatusConflict
	}
}

func requestTime(r RefreshRequest) time.Time { return time.Unix(r.Deadline, 0) }
