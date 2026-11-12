package chromeauth

import (
	"context"
	"errors"
	"net"
	"time"
)

type httpObserverKey struct{}

// HTTPObservation is in-process diagnostic metadata, not part of the v1 wire or
// session schema. A request observation covers one Do call, not its hidden
// transport attempts. Unknown send evidence must never authorize a retry.
type HTTPObservation struct {
	Phase            string
	Stage            string
	Category         string
	Status           int
	Elapsed          time.Duration
	ResponseReceived bool
	SendEvidence     string
}

// WithHTTPObserver composes observers so counting does not discard diagnostics.
func WithHTTPObserver(ctx context.Context, observer func(HTTPObservation)) context.Context {
	previous, _ := ctx.Value(httpObserverKey{}).(func(HTTPObservation))
	return context.WithValue(ctx, httpObserverKey{}, func(event HTTPObservation) {
		if previous != nil {
			previous(event)
		}
		if observer != nil {
			observer(event)
		}
	})
}

func observeHTTP(ctx context.Context, event HTTPObservation) {
	if observer, ok := ctx.Value(httpObserverKey{}).(func(HTTPObservation)); ok && observer != nil {
		observer(event)
	}
}

// oauthFailure deliberately does not retain the underlying error, URL, server
// status text, credentials or their hashes. Categories describe failure, never
// prove that all authentication attempts were unstarted.
type oauthFailure struct{ category string }

func (e *oauthFailure) Error() string { return "Chrome OAuth: " + e.category }
func (e *oauthFailure) Is(target error) bool {
	return target == ErrCredentialsRejected && e.category == "MATERIAL_REJECTED" ||
		target == ErrDeviceKeyUnavailable && e.category == "DEVICE_KEY_FAILURE" ||
		target == context.Canceled && e.category == "CANCELED" ||
		target == context.DeadlineExceeded && e.category == "TIMEOUT"
}

var ErrDeviceKeyUnavailable = errors.New("Chrome OAuth device key unavailable")

func oauthError(category string) error { return &oauthFailure{category: category} }

func oauthCategory(err error) string {
	if err == nil {
		return "OK"
	}
	var safe *oauthFailure
	if errors.As(err, &safe) {
		return safe.category
	}
	return "UNKNOWN_FAILURE"
}

func transportCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "TIMEOUT"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "DNS_FAILURE"
	}
	// EOF, proxy, TLS and partial-write errors remain transport failures.
	// Neither their strings nor the last connection attempt prove no send.
	return "TRANSPORT_FAILURE"
}
