package aistudio

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type outputLifetimeKey struct{}
type outputLifetime struct{ id, purpose string }

var outputLifetimeSequence atomic.Uint64

func contextWithOutputLifetime(ctx context.Context) context.Context {
	return context.WithValue(ctx, outputLifetimeKey{}, outputLifetime{id: fmt.Sprintf("output-%d", outputLifetimeSequence.Add(1)), purpose: GenerationPurpose(ctx)})
}

// generationTiming freezes a monotonic native-send -> first decoded terminal
// interval. Usage may arrive later, but cannot move either clock boundary.
type generationTiming struct {
	mu              sync.Mutex
	value           OutputTiming
	send, end       time.Time
	clock           func() time.Time
	ended, complete bool
	countsCovered   bool
	fault           string
}

func newGenerationTiming(ctx context.Context, attempt GenerationAttempt) *generationTiming {
	lifetime, ok := ctx.Value(outputLifetimeKey{}).(outputLifetime)
	if !ok {
		lifetime = contextWithOutputLifetime(ctx).Value(outputLifetimeKey{}).(outputLifetime)
	}
	timing := &generationTiming{clock: time.Now, value: OutputTiming{
		Version: OutputTimingVersion, Method: OutputTimingMethod, AttemptID: attempt.ID, RequestID: attempt.RequestID,
		LifecycleID: lifetime.id, Purpose: lifetime.purpose, Status: OutputTimingUnavailable, TokenSource: "upstream",
	}}
	if generationRPCChannel(attempt.Method) == ChannelAssistant {
		timing.fault = "unsupported_assistant_generation_semantics"
	}
	return timing
}
func (timing *generationTiming) markSend() {
	if timing == nil {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if timing.send.IsZero() {
		timing.send = timing.clock()
	}
}
func hasNonThinkingOutput(event Event) bool {
	switch event.Kind {
	case EventText:
		return event.Text != ""
	case EventToolCall:
		return event.ToolCall != nil
	case EventExecutableCode:
		return event.ExecutableCode != nil
	case EventCodeExecutionResult:
		return event.CodeExecutionResult != nil
	case EventMedia:
		return event.Media != nil
	}
	return false
}
func (timing *generationTiming) noteEvent(event Event) {
	if timing == nil || !hasNonThinkingOutput(event) {
		return
	}
	timing.mu.Lock()
	timing.value.HasOutput = true
	timing.mu.Unlock()
}
func (timing *generationTiming) observe(events []Event, counts *GenerationCounts, err error) {
	if timing == nil {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if err != nil {
		timing.fault = "invalid_frame"
	}
	generatedOutput := false
	for _, event := range events {
		if hasNonThinkingOutput(event) || event.Kind == EventReasoning && event.Text != "" {
			generatedOutput = true
		}
	}
	if generatedOutput {
		if !timing.end.IsZero() {
			timing.fault = "output_after_terminal"
		}
		timing.countsCovered = false
	}
	if counts != nil {
		timing.value.Counts = *counts
		timing.countsCovered = true
	}
	for _, event := range events {
		if hasNonThinkingOutput(event) {
			timing.value.HasOutput = true
		}
		if event.Kind == EventFinish && timing.end.IsZero() {
			timing.end = timing.clock()
		}
	}
}
func (timing *generationTiming) finish(complete bool) {
	if timing == nil {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if timing.ended {
		if !complete {
			timing.complete = false
		}
		return
	}
	timing.ended, timing.complete = true, complete
}
func (timing *generationTiming) ensureEnded() {
	if timing == nil {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if !timing.ended {
		timing.ended, timing.complete = true, false
	}
}
func (timing *generationTiming) snapshot() *OutputTiming {
	if timing == nil {
		return nil
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	value := timing.value
	value.Terminal, value.Complete = timing.ended, timing.complete
	value.GeneratedTokens, value.CountSource, value.Reason = value.Counts.generated()
	if !timing.send.IsZero() && !timing.end.IsZero() {
		value.DurationMS = float64(timing.end.Sub(timing.send)) / float64(time.Millisecond)
	}
	switch {
	case timing.fault != "":
		value.Status, value.Reason = OutputTimingUnavailable, timing.fault
	case !timing.ended || !timing.complete:
		value.Status, value.Reason = OutputTimingIncomplete, "incomplete_stream"
	case timing.send.IsZero() || timing.end.IsZero():
		value.Status, value.Reason = OutputTimingUnavailable, "missing_generation_clock"
	case value.DurationMS <= 0:
		value.Status, value.Reason = OutputTimingUnavailable, "invalid_generation_clock"
	case !timing.countsCovered:
		value.Status, value.Reason = OutputTimingUnavailable, "missing_final_generation_count"
	case value.Reason != "":
		value.Status = OutputTimingUnavailable
	case value.GeneratedTokens <= 0:
		value.Status, value.Reason = OutputTimingUnavailable, "no_generation_count"
	default:
		value.Status, value.Reason = OutputTimingMeasured, ""
	}
	return &value
}

// Protocol completeness is kept separate from the first native terminal clock.
type generationDecoderTiming struct {
	timing   *generationTiming
	complete bool
}

func (attempt *physicalAttempt) outputDecoder() *generationDecoderTiming {
	if attempt == nil {
		return nil
	}
	return &generationDecoderTiming{timing: attempt.output}
}
func (attempt *physicalAttempt) finishOutput(timing *generationDecoderTiming, err error, finished bool) {
	if attempt == nil {
		return
	}
	attempt.output.finish(err == nil && finished && timing != nil && timing.complete)
	attempt.update(func(*GenerationAttempt) {})
}
func (timing *generationDecoderTiming) observe(events []Event, counts *GenerationCounts, err error) {
	if timing != nil {
		timing.timing.observe(events, counts, err)
	}
}
