package aistudio

import "math"

const (
	OutputTimingVersion = 1
	OutputTimingMethod  = "upstream_non_thinking_window_v1"

	OutputTimingMeasured     = "measured"
	OutputTimingUnavailable  = "unavailable"
	OutputTimingIncomplete   = "incomplete"
	OutputTimingUnattributed = "unattributed"
)

// OutputTiming is bounded, body-free evidence for one physical attempt's
// observed non-thinking output window. The first batch is the counter baseline,
// not an extra batch of tokens to divide by the time after its arrival.
// No response text, argument, signature, wall-clock timestamp or per-frame list
// belongs here. This is not a provider model-identity or GPU-clock assertion.
type OutputTiming struct {
	Version           int     `json:"version"`
	Method            string  `json:"method"`
	AttemptID         string  `json:"attempt_id"`
	RequestID         string  `json:"request_id"`
	LifecycleID       string  `json:"lifecycle_id"`
	Purpose           string  `json:"purpose"`
	Status            string  `json:"status"`
	Reason            string  `json:"reason,omitempty"`
	Complete          bool    `json:"complete"`
	Terminal          bool    `json:"terminal"`
	HasOutput         bool    `json:"has_output"`
	ProgressBatches   int     `json:"progress_batches"`
	StartOutputTokens int64   `json:"start_output_tokens"`
	EndOutputTokens   int64   `json:"end_output_tokens"`
	TokenSource       string  `json:"token_source"`
	DurationMS        float64 `json:"duration_ms"`
}

// TPS validates and computes the sole public numeric projection. Internal
// statuses/reasons never become a UI quality badge, tooltip or route warning.
func (timing *OutputTiming) TPS() *float64 {
	if timing == nil || timing.Version != OutputTimingVersion || timing.Method != OutputTimingMethod ||
		timing.Status != OutputTimingMeasured || timing.Reason != "" || !timing.Complete || !timing.Terminal || !timing.HasOutput ||
		timing.AttemptID == "" || timing.LifecycleID == "" || (timing.Purpose != "reply" && timing.Purpose != "compaction") ||
		timing.TokenSource != "upstream" || timing.ProgressBatches < 2 ||
		timing.StartOutputTokens < 0 || timing.EndOutputTokens <= timing.StartOutputTokens ||
		timing.DurationMS <= 0 || math.IsNaN(timing.DurationMS) || math.IsInf(timing.DurationMS, 0) {
		return nil
	}
	value := float64(timing.EndOutputTokens-timing.StartOutputTokens) * 1000 / timing.DurationMS
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}
