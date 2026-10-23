package authhelper

import (
	"context"
	"sync/atomic"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

type authCounts struct {
	sent      atomic.Int64
	responses atomic.Int64
}

func countAuthentication(ctx context.Context) (context.Context, *authCounts) {
	counts := new(authCounts)
	return chromeauth.WithHTTPObserver(ctx, func(event chromeauth.HTTPObservation) {
		if event.Sent {
			counts.sent.Add(1)
		} else {
			counts.responses.Add(1)
		}
	}), counts
}
func (c *authCounts) snapshot() *aistudio.AuthHTTPCounts {
	return &aistudio.AuthHTTPCounts{Sent: int(c.sent.Load()), Responses: int(c.responses.Load())}
}
