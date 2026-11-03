package api

import "github.com/Mag1cFall/AIStudio2API/internal/aistudio"

// outputTimingFromAttempts projects one reply's evidence without borrowing the
// summary's generation interval or combining candidate/continuation output. Existing attempt
// upserts own ordering and de-duplication; this projection retains no new list.
func outputTimingFromAttempts(attempts []RequestAttempt) *aistudio.OutputTiming {
	var selected *aistudio.OutputTiming
	var replyLifecycle string
	unattributed := func(reason string) *aistudio.OutputTiming {
		return &aistudio.OutputTiming{
			Version: aistudio.OutputTimingVersion, Method: aistudio.OutputTimingMethod,
			Purpose: "reply", Status: aistudio.OutputTimingUnattributed,
			Reason: reason, HasOutput: true,
		}
	}
	for _, attempt := range attempts {
		if attempt.Kind != AttemptKindGeneration {
			continue
		}
		timing := attempt.OutputTiming
		if timing == nil {
			if attempt.Sent != nil && !*attempt.Sent && attempt.Stage == "preparation_failed" && attempt.Error != "" {
				continue
			}
			return unattributed("missing_attempt_evidence")
		}
		if timing.Purpose == "compaction" {
			continue
		}
		if timing.Purpose != "reply" || timing.LifecycleID == "" || timing.AttemptID != attempt.ID || timing.RequestID != attempt.RequestID {
			return unattributed("attempt_identity_mismatch")
		}
		if !timing.Terminal {
			return unattributed("reply_lifecycle_unresolved")
		}
		if !timing.HasOutput && attempt.Error != "" {
			continue
		}
		if replyLifecycle != "" && replyLifecycle != timing.LifecycleID {
			return unattributed("multiple_reply_lifecycles")
		}
		replyLifecycle = timing.LifecycleID
		if !timing.HasOutput {
			continue
		}
		if attempt.Sent == nil || !*attempt.Sent || attempt.ResponseReceived == nil || !*attempt.ResponseReceived ||
			attempt.StatusCode < 200 || attempt.StatusCode >= 300 || attempt.Error != "" {
			return unattributed("attempt_not_successful")
		}
		if selected != nil {
			return unattributed("multiple_output_attempts")
		}
		value := *timing
		selected = &value
	}
	return selected
}

// ObservedOutputTPS is the only numeric record projection. A late successful
// native snapshot cannot make an HTTP failure/cancellation look measured.
func (record UsageRecord) ObservedOutputTPS() *float64 {
	if record.Status < 200 || record.Status >= 300 || record.Error != "" ||
		record.OutputTiming == nil || record.OutputTiming.Purpose != "reply" {
		return nil
	}
	switch record.State {
	case "completed", "tool_calls", "limited", "blocked":
		return record.OutputTiming.TPS()
	default:
		return nil
	}
}
