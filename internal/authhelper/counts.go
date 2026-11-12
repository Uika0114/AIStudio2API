package authhelper

import (
	"context"
	"sync/atomic"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

type authCounts struct {
	responses atomic.Int64
}

func countAuthentication(ctx context.Context) (context.Context, *authCounts) {
	counts := new(authCounts)
	return chromeauth.WithHTTPObserver(ctx, counts.observe), counts
}

func (c *authCounts) observe(event chromeauth.HTTPObservation) {
	if (event.Phase == "challenge" || event.Phase == "assertion") && event.ResponseReceived {
		c.responses.Add(1)
	}
}

func (c *authCounts) snapshot() *aistudio.AuthHTTPCounts {
	// Keep the existing strict JSON shape. New counts are a response-confirmed
	// lower bound, not Do invocations or a complete count of transport attempts.
	// In particular zero does NOT prove NOT_STARTED; historical counts did not
	// prove sending either and are never upgraded into such evidence.
	responses := int(c.responses.Load())
	return &aistudio.AuthHTTPCounts{Sent: responses, Responses: responses}
}
