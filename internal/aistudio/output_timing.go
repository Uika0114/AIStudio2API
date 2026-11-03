package aistudio

import "math"

const (
	OutputTimingVersion      = 2
	OutputTimingMethod       = "upstream_generation_average_v1"
	OutputTimingMeasured     = "measured"
	OutputTimingUnavailable  = "unavailable"
	OutputTimingIncomplete   = "incomplete"
	OutputTimingUnattributed = "unattributed"
)

// GenerationCounts is a single raw upstream usage snapshot, never merged with
// cached Usage or the log projection (whose output already includes reasoning).
// Absent tool-use-prompt means zero in PG/Build; cache is included in input.
type GenerationCounts struct {
	Invalid        bool  `json:"invalid,omitempty"`
	Input          int64 `json:"input"`
	Output         int64 `json:"output"`
	Reasoning      int64 `json:"reasoning"`
	Total          int64 `json:"total"`
	ToolPrompt     int64 `json:"tool_prompt"`
	InputKnown     bool  `json:"input_known"`
	OutputKnown    bool  `json:"output_known"`
	ReasoningKnown bool  `json:"reasoning_known"`
	TotalKnown     bool  `json:"total_known"`
}

func (c GenerationCounts) generated() (int64, string, string) {
	if c.Invalid {
		return 0, "", "invalid_count"
	}
	if c.InputKnown && c.Input < 0 || c.OutputKnown && c.Output < 0 || c.ReasoningKnown && c.Reasoning < 0 || c.TotalKnown && c.Total < 0 || c.ToolPrompt < 0 {
		return 0, "", "negative_count"
	}
	independent := c.OutputKnown && c.ReasoningKnown
	derived := c.TotalKnown && c.InputKnown
	var sum, remainder int64
	if independent {
		if c.Output > math.MaxInt64-c.Reasoning {
			return 0, "", "count_overflow"
		}
		sum = c.Output + c.Reasoning
	}
	if derived {
		if c.Total < c.Input || c.Total-c.Input < c.ToolPrompt {
			return 0, "", "inconsistent_counts"
		}
		remainder = c.Total - c.Input - c.ToolPrompt
		if independent && remainder != sum || c.OutputKnown && remainder < c.Output || c.ReasoningKnown && remainder < c.Reasoning {
			return 0, "", "inconsistent_counts"
		}
	}
	if independent {
		return sum, "output_plus_reasoning", ""
	}
	if derived {
		return remainder, "total_minus_input_tool", ""
	}
	return 0, "", "missing_generation_count"
}

// OutputTiming is bounded body-free evidence. V1 fields remain for lossless
// historical cold reads, but TPS never projects the old non-thinking method.
// Duration includes inseparable upstream queue/prefill/network, not local setup
// or post-terminal cleanup; it is an overall average, not a GPU decode clock.
type OutputTiming struct {
	Version           int              `json:"version"`
	Method            string           `json:"method"`
	AttemptID         string           `json:"attempt_id"`
	RequestID         string           `json:"request_id"`
	LifecycleID       string           `json:"lifecycle_id"`
	Purpose           string           `json:"purpose"`
	Status            string           `json:"status"`
	Reason            string           `json:"reason,omitempty"`
	Complete          bool             `json:"complete"`
	Terminal          bool             `json:"terminal"`
	HasOutput         bool             `json:"has_output"`
	TokenSource       string           `json:"token_source"`
	DurationMS        float64          `json:"duration_ms"`
	GeneratedTokens   int64            `json:"generated_tokens,omitempty"`
	CountSource       string           `json:"count_source,omitempty"`
	Counts            GenerationCounts `json:"counts"`
	ProgressBatches   int              `json:"progress_batches,omitempty"`
	StartOutputTokens int64            `json:"start_output_tokens,omitempty"`
	EndOutputTokens   int64            `json:"end_output_tokens,omitempty"`
}

func (timing *OutputTiming) TPS() *float64 {
	if timing == nil || timing.Version != OutputTimingVersion || timing.Method != OutputTimingMethod ||
		timing.Status != OutputTimingMeasured || timing.Reason != "" || !timing.Complete || !timing.Terminal || !timing.HasOutput ||
		timing.AttemptID == "" || timing.LifecycleID == "" || (timing.Purpose != "reply" && timing.Purpose != "compaction") ||
		timing.TokenSource != "upstream" || timing.DurationMS <= 0 || math.IsNaN(timing.DurationMS) || math.IsInf(timing.DurationMS, 0) {
		return nil
	}
	generated, source, reason := timing.Counts.generated()
	if reason != "" || generated <= 0 || generated != timing.GeneratedTokens || source != timing.CountSource {
		return nil
	}
	value := float64(generated) * 1000 / timing.DurationMS
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}
