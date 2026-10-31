package authhelper

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type Client struct {
	endpoint string
	consumer string
	domains  []string
	http     *http.Client
}

// NewClient only accepts a loopback SSH endpoint and a verifying mutual-TLS config.
func NewClient(endpoint, consumer string, config *tls.Config, domains []string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" || consumer == "" {
		return nil, failure("INVALID_REQUEST")
	}
	if config == nil || config.InsecureSkipVerify || config.RootCAs == nil || config.ServerName == "" || len(config.Certificates) != 1 || config.MinVersion < tls.VersionTLS12 {
		return nil, failure("INVALID_REQUEST")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSClientConfig: config.Clone(), TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second, MaxConnsPerHost: 4, MaxIdleConns: 4, ForceAttemptHTTP2: false}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), consumer: consumer, domains: append([]string(nil), domains...), http: client}, nil
}

func (c *Client) Close()           { c.http.CloseIdleConnections() }
func (c *Client) Consumer() string { return c.consumer }

func (c *Client) rpc(ctx context.Context, path string, input any) (*Result, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, failure("INVALID_REQUEST")
	}
	limit := 100 * time.Second
	if path == "/v1/ack" {
		limit = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, failure("INVALID_REQUEST")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, failure("CANCELED")
		}
		return nil, failure("HELPER_UNAVAILABLE")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusConflict && response.StatusCode != http.StatusServiceUnavailable {
		return nil, failure("HELPER_UNAVAILABLE")
	}
	limited := io.LimitReader(response.Body, MaxBody+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > MaxBody {
		return nil, failure("HELPER_UNAVAILABLE")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Result
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, failure("HELPER_UNAVAILABLE")
	}
	if result.Code != "OK" {
		if result.Code == "NOT_STARTED" {
			request, ok := input.(RefreshRequest)
			if !ok || result.NotStarted == nil || *result.NotStarted != request {
				return nil, failure("IDENTITY_MISMATCH")
			}
			return nil, &notStartedError{receipt: request}
		}
		return nil, failure(safeCode(failure(result.Code)))
	}
	if response.StatusCode != http.StatusOK {
		return nil, failure("HELPER_UNAVAILABLE")
	}
	return &result, nil
}

func (c *Client) AckSession(ctx context.Context, r *aistudio.RemoteSession) error {
	if r == nil || r.Consumer != c.consumer || r.Ack == nil {
		return failure("IDENTITY_MISMATCH")
	}
	a := r.Ack
	result, err := c.rpc(ctx, "/v1/ack", AckRequest{Version: Protocol, Handle: r.Handle, Epoch: a.Epoch, OperationID: a.ID, CookieRevision: a.Cookie, MaterialVersion: a.Material, Mode: a.Mode})
	code := "OK"
	if err != nil {
		code = safeCode(err)
	}
	reportRecovery(ctx, "ack", code, &aistudio.SessionState{Remote: r}, true)
	if err != nil {
		return err
	}
	if result.Session != nil {
		return failure("INVALID_REQUEST")
	}
	return nil
}

// PrepareRecovery durably records the operation before sending. The caller owns an
// account lease/storage lock and commits cookies only after Signer/runtime checks.
func (c *Client) PrepareRecovery(ctx context.Context, state *aistudio.StorageState, path, reason string, takeBudget func(string) error) error {
	if ctx.Err() != nil {
		return failure("CANCELED")
	}
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil || meta.Remote.Consumer != c.consumer {
		return failure("IDENTITY_MISMATCH")
	}
	r := meta.Remote
	if r.EnrollmentRequired && reason != "bootstrap" && r.Pending == nil {
		return failure("BOOTSTRAP_REQUIRED")
	}
	if r.Ack != nil {
		if err := c.AckSession(ctx, r); err != nil {
			return err
		}
		r.Ack = nil
		if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, path, state) != nil {
			return failure("STATE_UNAVAILABLE")
		}
		meta, _ = state.Session()
		r = meta.Remote
	}
	created := r.Pending == nil
	if created {
		if reason != "bootstrap" && reason != "reauth" {
			return failure("INVALID_REQUEST")
		}
		deadline := time.Now().Add(90 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if deadline.Unix() <= time.Now().Unix()+1 {
			return failure("CANCELED")
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return failure("STATE_UNAVAILABLE")
		}
		r.Pending = &aistudio.SessionOperation{ID: hex.EncodeToString(id[:]), Reason: reason, Epoch: r.Epoch, BaseCookie: r.CookieRevision, BaseMaterial: r.MaterialVersion, StateRevision: meta.Revision + 1, Deadline: deadline.Unix()}
		if reason == "reauth" {
			if takeBudget == nil {
				return failure("RECOVERY_BUDGET_REQUIRED")
			}
			if err := takeBudget(r.Pending.ID); err != nil {
				return err
			}
		}
		r.RecoveryRequired = true
		if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, path, state) != nil {
			return failure("STATE_UNAVAILABLE")
		}
		meta, _ = state.Session()
		r = meta.Remote
	}
	request := operationRequest(r.Pending, r.Handle)
	request.LookupOnly = !created
	result, err := c.pendingResult(ctx, state, path, meta, !created)
	if err != nil {
		var notStarted *notStartedError
		if errors.As(err, &notStarted) {
			if e := retireNotStarted(ctx, state, path, meta, notStarted.receipt, false); e != nil {
				return e
			}
		}
		return err
	}
	if err := c.validateRecoveryResult(meta, request, result); err != nil {
		reportRecovery(ctx, "result_validation", safeCode(err), meta, !created)
		return err
	}
	// A newer local snapshot is sufficient for lookup and metadata-only
	// retirement, never permission to overwrite independently updated cookies.
	if r.Pending.StateRevision != meta.Revision {
		reportRecovery(ctx, "projection_blocked", "VERSION_CONFLICT", meta, !created)
		return failure("VERSION_CONFLICT")
	}
	if err := aistudio.CheckSessionRevision(path, *state); err != nil {
		reportRecovery(ctx, "projection_blocked", "VERSION_CONFLICT", meta, !created)
		return err
	}
	if ctx.Err() != nil {
		return failure("CANCELED")
	}
	value := result.Session
	state.Cookies = append([]aistudio.StateCookie(nil), value.Cookies...)
	r.CookieRevision, r.MaterialVersion = value.CookieRevision, value.MaterialVersion
	r.Pending = nil
	r.RecoveryRequired = false
	if request.Reason == "bootstrap" {
		r.EnrollmentRequired = false
	}
	r.Ack = &aistudio.SessionAck{ID: request.OperationID, Epoch: r.Epoch, Cookie: r.CookieRevision, Material: r.MaterialVersion}
	reportRecovery(ctx, "projection_prepared", "OK", meta, !created)
	return state.SetSession(meta)
}
