package aistudio

import (
	"context"
	"sync/atomic"
	"time"
)

// GenerationAttempt is a credential/body-free snapshot of one physical send
// attempt. Sent means the send boundary was entered, not proof of execution.
// Repeated snapshots with the same ID update one attempt, not another request.
type GenerationAttempt struct {
	ID                string    `json:"id"`
	Revision          uint64    `json:"revision"`
	ContinuationRound int       `json:"continuation_round,omitempty"`
	RequestID         string    `json:"request_id"`
	ParentID          string    `json:"parent_id,omitempty"`
	RequestedModel    string    `json:"requested_model"`
	BaseModel         string    `json:"base_model"`
	RequestedChannel  Channel   `json:"requested_channel,omitempty"`
	Channel           Channel   `json:"channel"`
	AntiTruncation    bool      `json:"anti_truncation,omitempty"`
	AccountID         string    `json:"account_id"`
	Method            string    `json:"method"`
	Mode              string    `json:"mode"`
	Reason            string    `json:"reason,omitempty"`
	Stage             string    `json:"stage"`
	Sent              bool      `json:"sent"`
	ResponseReceived  bool      `json:"response_received"`
	StatusCode        int       `json:"status_code,omitempty"`
	DurationMS        int64     `json:"duration_ms"`
	StartedAt         time.Time `json:"started_at"`
	Error             string    `json:"error,omitempty"`
	Usage             *Usage    `json:"usage,omitempty"`
	UsageSource       string    `json:"usage_source"`
}

type generationAttemptObserverKey struct{}
type generationProgressObserverKey struct{}

// ContextWithGenerationAttemptObserver receives attempt snapshots. The callback
// may run concurrently (multi-choice or stream close); it must synchronize itself.
func ContextWithGenerationAttemptObserver(ctx context.Context, observer func(GenerationAttempt)) context.Context {
	return context.WithValue(ctx, generationAttemptObserverKey{}, observer)
}

func reportGenerationAttempt(ctx context.Context, attempt GenerationAttempt) {
	if callback, ok := ctx.Value(generationAttemptObserverKey{}).(func(GenerationAttempt)); ok && callback != nil {
		callback(attempt)
	}
}

// ContextWithGenerationProgressObserver observes raw semantic events before
// transport projection/buffering. It prevents hidden output from enabling replay.
func ContextWithGenerationProgressObserver(ctx context.Context, observer func()) context.Context {
	return context.WithValue(ctx, generationProgressObserverKey{}, observer)
}

func observeGenerationProgress(ctx context.Context) {
	if callback, ok := ctx.Value(generationProgressObserverKey{}).(func()); ok && callback != nil {
		callback()
	}
}

var generationAttemptSequence atomic.Uint64
