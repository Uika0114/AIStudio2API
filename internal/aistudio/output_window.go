package aistudio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type outputLifetimeKey struct{}
type outputLifetime struct{ id, purpose string }

var outputLifetimeSequence atomic.Uint64

func contextWithOutputLifetime(ctx context.Context) context.Context {
	return context.WithValue(ctx, outputLifetimeKey{}, outputLifetime{
		id: fmt.Sprintf("output-%d", outputLifetimeSequence.Add(1)), purpose: GenerationPurpose(ctx),
	})
}

// A source marker is deliberately internal. A browser's locally buffered chunks
// must not be mistaken for independently arriving network reads.
type outputReadSourceBody struct{ io.ReadCloser }

func (*outputReadSourceBody) OutputReadSourceKnown() bool { return true }
func knownOutputReadSource(reader io.Reader) bool {
	source, ok := reader.(interface{ OutputReadSourceKnown() bool })
	return ok && source.OutputReadSourceKnown()
}

type outputReadBatch struct {
	id uint64
	at time.Time
}

type outputPendingBatch struct {
	batch         outputReadBatch
	text, covered bool
	count         int64
}

// outputWindow retains only one contiguous candidate window, not a transcript,
// frame list, text hash or an aggregation of unrelated output segments.
type outputWindow struct {
	mu                 sync.Mutex
	value              OutputTiming
	pending            outputPendingBatch
	first, last        outputReadBatch
	start, end         int64
	batches            int
	lastCounter        int64
	counterKnown       bool
	lastReasoning      int64
	reasoningKnown     bool
	unpairedText       bool
	unattributedGrowth bool
	fault, resetReason string
	ended, complete    bool
}

func newOutputWindow(ctx context.Context, attempt GenerationAttempt) *outputWindow {
	lifetime, ok := ctx.Value(outputLifetimeKey{}).(outputLifetime)
	if !ok {
		ctx = contextWithOutputLifetime(ctx)
		lifetime = ctx.Value(outputLifetimeKey{}).(outputLifetime)
	}
	return &outputWindow{value: OutputTiming{
		Version: OutputTimingVersion, Method: OutputTimingMethod,
		AttemptID: attempt.ID, RequestID: attempt.RequestID, LifecycleID: lifetime.id,
		Purpose: lifetime.purpose, Status: OutputTimingUnavailable, TokenSource: "upstream",
	}, resetReason: "insufficient_progress"}
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

// noteEvent also covers unsupported snapshot/unary paths. Seeing output is not
// sufficient evidence that its generation window can be measured.
func (window *outputWindow) noteEvent(event Event) {
	if window == nil || !hasNonThinkingOutput(event) {
		return
	}
	window.mu.Lock()
	window.value.HasOutput = true
	window.mu.Unlock()
}

func (window *outputWindow) unavailable(reason string) {
	if window == nil {
		return
	}
	window.mu.Lock()
	window.fault = reason
	window.mu.Unlock()
}

func (window *outputWindow) reset(reason string) {
	window.pending = outputPendingBatch{}
	window.first, window.last = outputReadBatch{}, outputReadBatch{}
	window.start, window.end, window.batches = 0, 0, 0
	window.resetReason = reason
	window.unpairedText, window.unattributedGrowth = false, false
}

func (window *outputWindow) flush() {
	pending := window.pending
	window.pending = outputPendingBatch{}
	if !pending.text {
		return
	}
	if !pending.covered {
		// Pure text between two covered boundaries need not carry a counter.
		// Do not use THIS batch as a boundary or borrow an earlier count.
		window.resetReason = "unpaired_output_count"
		return
	}
	if window.first.id == 0 {
		window.first, window.last = pending.batch, pending.batch
		window.start, window.end, window.batches = pending.count, pending.count, 1
		return
	}
	if pending.count <= window.end {
		return
	} // repeated/unchanged counters are not progress
	if pending.batch.id <= window.last.id || !pending.batch.at.After(window.last.at) {
		window.fault = "invalid_batch_clock"
		return
	}
	window.last, window.end = pending.batch, pending.count
	window.batches++
}

func (window *outputWindow) observeFrame(events []Event, count int64, known bool, reasoning *int64, batch outputReadBatch, paired bool, decodeErr error) {
	if window == nil {
		return
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	if window.ended {
		return
	}
	if decodeErr != nil {
		window.fault = "invalid_frame"
		return
	}
	text, phase := false, false
	for _, event := range events {
		if hasNonThinkingOutput(event) {
			window.value.HasOutput = true
		}
		switch event.Kind {
		case EventText:
			if event.Transcript != nil {
				phase, text = true, false
			} else {
				text = text || event.Text != ""
			}
		case EventReasoning:
			if event.Text != "" {
				phase, text = true, false
			}
		case EventToolCall, EventExecutableCode, EventCodeExecutionResult, EventMedia:
			phase, text = true, false // preserve the last interruption's semantic order
		}
	}
	// Fresh optional R is an interruption signal, not part of the TPS numerator.
	// Missing R is not zero; first presence cannot prove progress in this window.
	if reasoning != nil && *reasoning >= 0 {
		phase = phase || window.reasoningKnown && *reasoning > window.lastReasoning
		window.lastReasoning, window.reasoningKnown = *reasoning, true
	}
	if phase {
		window.reset("non_output_phase")
	}
	if text && window.unattributedGrowth {
		window.reset("unattributed_count_progress")
	}
	if known {
		if count < 0 || window.counterKnown && count < window.lastCounter {
			window.fault = "output_counter_reset"
		}
		currentBatchText := paired && window.pending.batch.id == batch.id && window.pending.text
		if !text && !phase && window.counterKnown && count > window.lastCounter && !window.unpairedText && !currentBatchText {
			// Keep an already covered end, but do not include unexplained
			// metadata-only growth in a later text window.
			window.unattributedGrowth = true
		}
		window.lastCounter, window.counterKnown = count, true
		window.unpairedText = false
	} else if text {
		window.unpairedText = true
	}
	if !paired {
		if text {
			window.reset("unpaired_output_progress")
		}
		return
	}
	if window.pending.batch.id != 0 && window.pending.batch.id != batch.id {
		window.flush()
	}
	if window.pending.batch.id == 0 {
		window.pending.batch = batch
	}
	if text {
		window.pending.text = true
		window.pending.covered = known
	}
	if known {
		// A later fresh snapshot in this SAME read may cover its earlier text.
		// A snapshot in a different read cannot be moved back to that text.
		window.pending.count = count
		window.pending.covered = true
	}
}

func (window *outputWindow) finish(complete bool) {
	if window == nil {
		return
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	if window.ended {
		if !complete {
			window.complete = false
		}
		return
	}
	window.flush()
	window.ended, window.complete = true, complete
}

func (window *outputWindow) ensureEnded() {
	if window == nil {
		return
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	if !window.ended {
		window.flush()
		window.ended, window.complete = true, false
	}
}

func (window *outputWindow) snapshot() *OutputTiming {
	if window == nil {
		return nil
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	value := window.value
	value.Terminal, value.Complete = window.ended, window.complete
	value.ProgressBatches = window.batches
	value.StartOutputTokens, value.EndOutputTokens = window.start, window.end
	if window.first.id != 0 && window.last.id != 0 {
		value.DurationMS = float64(window.last.at.Sub(window.first.at)) / float64(time.Millisecond)
	}
	switch {
	case !window.ended || !window.complete:
		value.Status, value.Reason = OutputTimingIncomplete, "incomplete_stream"
	case window.fault != "":
		value.Status, value.Reason = OutputTimingUnavailable, window.fault
	case window.batches < 2 || window.end <= window.start || value.DurationMS <= 0:
		value.Status, value.Reason = OutputTimingUnavailable, window.resetReason
	default:
		value.Status, value.Reason = OutputTimingMeasured, ""
	}
	return &value
}
