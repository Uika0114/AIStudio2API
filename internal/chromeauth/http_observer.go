package chromeauth

import "context"

type httpObserverKey struct{}

// HTTPObservation contains fixed stage/status metadata, never URL, headers or body.
type HTTPObservation struct {
	Stage  string
	Sent   bool
	Status int
}

func WithHTTPObserver(ctx context.Context, observer func(HTTPObservation)) context.Context {
	return context.WithValue(ctx, httpObserverKey{}, observer)
}
func observeHTTP(ctx context.Context, stage string, sent bool, status int) {
	if observer, ok := ctx.Value(httpObserverKey{}).(func(HTTPObservation)); ok && observer != nil {
		observer(HTTPObservation{Stage: stage, Sent: sent, Status: status})
	}
}
