package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebAccess is a page observation, never an authentication or dispatch verdict.
type WebAccess string

const (
	WebAccessUnknown    WebAccess = "unknown"
	WebAccessNormal     WebAccess = "normal"
	WebAccessRestricted WebAccess = "restricted"
)

type BootstrapMode string

const (
	BootstrapLegacy             BootstrapMode = "legacy"
	BootstrapRestrictedFallback BootstrapMode = "restricted_fallback"
)

// WebAccessCheck contains no URL, cookies, key or upstream body.
type WebAccessCheck struct {
	Access    WebAccess
	CheckedAt time.Time
}

// WebAccessObservation is process-local last-known evidence. A failed check does
// not make the last definitive observation fresh or change account eligibility.
type WebAccessObservation struct {
	Access     WebAccess
	ObservedAt time.Time
	LastCheck  WebAccessCheck
}

func (o *WebAccessObservation) Observe(check WebAccessCheck) {
	if check.CheckedAt.Before(o.LastCheck.CheckedAt) {
		return
	}
	if check.Access != WebAccessNormal && check.Access != WebAccessRestricted {
		check.Access = WebAccessUnknown
	}
	o.LastCheck = check
	if check.Access != WebAccessUnknown {
		o.Access, o.ObservedAt = check.Access, check.CheckedAt
	} else if o.Access == "" {
		o.Access = WebAccessUnknown
	}
}

type webAccessObserverKey struct{}

// ContextWithWebAccessObserver installs a synchronous, credential-free callback.
// Callers own account identity/start-token checks and synchronization.
func ContextWithWebAccessObserver(ctx context.Context, observer func(WebAccessCheck)) context.Context {
	return context.WithValue(ctx, webAccessObserverKey{}, observer)
}

func reportWebAccess(ctx context.Context, check WebAccessCheck) {
	if observer, ok := ctx.Value(webAccessObserverKey{}).(func(WebAccessCheck)); ok && observer != nil {
		observer(check)
	}
}

type pageLoadResult struct {
	access WebAccess
	apiKey string
}

func isServiceRestrictedLocation(location *url.URL) bool {
	return location != nil && strings.EqualFold(location.Scheme, "https") &&
		strings.EqualFold(location.Hostname(), "accounts.google.com") &&
		location.User == nil && !strings.HasSuffix(location.Host, ":") &&
		(location.Port() == "" || location.Port() == "443") &&
		location.EscapedPath() == "/info/servicerestricted"
}

// bootstrapAPIKey preserves normal navigation. Only an exact restriction gets
// one public discovery using a new, account-free client on the effective proxy.
func (runtime *goWAARuntime) bootstrapAPIKey(ctx context.Context, newClient func(string) (*http.Client, error)) (string, error) {
	page, err := runtime.loadPage(ctx)
	reportWebAccess(ctx, WebAccessCheck{Access: page.access, CheckedAt: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if page.access == WebAccessNormal {
		runtime.bootstrapMode = BootstrapLegacy
		return page.apiKey, nil
	}
	if page.access != WebAccessRestricted {
		return "", errors.New("AI Studio 页面状态未知")
	}
	client, err := newClient(runtime.options.Proxy)
	if err != nil {
		return "", safeBootstrapFailure("public_discovery_client", err)
	}
	if client == nil {
		return "", errors.New("WAA bootstrap public_discovery_client unavailable")
	}
	defer client.CloseIdleConnections()
	// A task-specific client must never inherit account cookies, even if a
	// future factory starts supplying a jar by default.
	anonymous := *client
	anonymous.Jar = nil
	headers, err := DiscoverPublicHeaders(ctx, &anonymous)
	if err != nil {
		return "", safeBootstrapFailure("public_discovery", err)
	}
	apiKey := headers.Get("X-Goog-Api-Key")
	if apiKey == "" {
		return "", errors.New("WAA bootstrap public_discovery missing key")
	}
	runtime.bootstrapMode = BootstrapRestrictedFallback
	return apiKey, nil
}

// Transport/parser errors may embed URLs, proxy credentials or raw bodies.
// Preserve transport classification and cancellation, never raw error details.
func safeBootstrapFailure(stage string, err error) error {
	safe := fmt.Errorf("WAA bootstrap %s failed", stage)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			safe = fmt.Errorf("WAA bootstrap %s: %w", stage, cause)
			break
		}
	}
	var transport *url.Error
	if errors.As(err, &transport) {
		return &url.Error{Op: "request", Err: safe}
	}
	return safe
}

func bootstrapRPCError(method string, status int, body []byte) *RPCError {
	decoded := DecodeRPCError(method, status, body)
	return &RPCError{Method: method, StatusCode: status, Code: decoded.Code,
		Message: "WAA bootstrap RPC rejected"}
}

// BootstrapMode belongs to the immutable worker, not the account's latest page
// observation. Non-Go workers have no new mode and keep their previous behavior.
func (worker *NativeWorker) BootstrapMode() BootstrapMode {
	return worker.bootstrapMode
}
