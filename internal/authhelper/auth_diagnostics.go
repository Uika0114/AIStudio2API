package authhelper

import (
	"context"

	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

// AuthenticationProgress never travels in v1 replies or durable auth JSON.
// HTTP is safe fixed metadata; version numbers belong to the original intent,
// not a claim that a result was committed.
type AuthenticationProgress struct {
	OperationID  string
	Epoch        uint64
	BaseCookie   uint64
	BaseMaterial uint64
	HTTP         chromeauth.HTTPObservation
}

type authenticationObserverKey struct{}

func WithAuthenticationObserver(ctx context.Context, observe func(AuthenticationProgress)) context.Context {
	return context.WithValue(ctx, authenticationObserverKey{}, observe)
}

func reportAuthentication(ctx context.Context, request RefreshRequest, event chromeauth.HTTPObservation) {
	observe, _ := ctx.Value(authenticationObserverKey{}).(func(AuthenticationProgress))
	if observe != nil {
		observe(AuthenticationProgress{OperationID: request.OperationID, Epoch: request.Epoch, BaseCookie: request.BaseCookie, BaseMaterial: request.BaseMaterial, HTTP: event})
	}
}

func withAuthenticationDiagnostics(ctx context.Context, request RefreshRequest) context.Context {
	return chromeauth.WithHTTPObserver(ctx, func(event chromeauth.HTTPObservation) {
		reportAuthentication(ctx, request, event)
	})
}
