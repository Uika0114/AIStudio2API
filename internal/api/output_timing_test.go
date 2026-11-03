package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func fixtureOutputAttempt(id, purpose string) RequestAttempt {
	sent, response := true, true
	return RequestAttempt{
		Kind: AttemptKindGeneration, ID: id, RequestID: "fixture/reused-id",
		Sent: &sent, ResponseReceived: &response, StatusCode: 200,
		OutputTiming: &aistudio.OutputTiming{
			Version: aistudio.OutputTimingVersion, Method: aistudio.OutputTimingMethod,
			AttemptID: id, RequestID: "fixture/reused-id", LifecycleID: "fixture/lifetime", Purpose: purpose,
			Status: aistudio.OutputTimingMeasured, Complete: true, Terminal: true, HasOutput: true,
			GeneratedTokens: 520, CountSource: "output_plus_reasoning",
			Counts:      aistudio.GenerationCounts{Output: 420, Reasoning: 100, OutputKnown: true, ReasoningKnown: true},
			TokenSource: "upstream", DurationMS: 8000,
		},
	}
}

func TestOutputTimingRootProjection(t *testing.T) {
	main := fixtureOutputAttempt("reply", "reply")
	summary := fixtureOutputAttempt("summary", "compaction")
	prior := fixtureOutputAttempt("rejected-before-output", "reply")
	prior.OutputTiming.HasOutput = false
	prior.OutputTiming.Status = aistudio.OutputTimingIncomplete
	prior.Error = "upstream HTTP 429"
	prior.StatusCode, prior.Stage = 429, "http_error"
	notSent := false
	preparing := RequestAttempt{Kind: AttemptKindGeneration, Sent: &notSent, Stage: "preparation_failed", Error: "fixture preparation"}
	ack := RequestAttempt{Kind: "authentication_ack", DurationMS: 90000}
	legacy := RequestAttempt{Kind: AttemptKindLegacy, Usage: &aistudio.Usage{OutputTokens: 500}}
	got := outputTimingFromAttempts([]RequestAttempt{summary, prior, preparing, ack, legacy, main})
	if got == nil || got.TPS() == nil || *got.TPS() != 65 || got.AttemptID != main.ID {
		t.Fatalf("root did not select only the final reply: %+v", got)
	}
	got.DurationMS = 1
	if main.OutputTiming.DurationMS != 8000 {
		t.Fatal("root projection aliases the attempt evidence")
	}
	if got := outputTimingFromAttempts([]RequestAttempt{summary}); got != nil {
		t.Fatalf("summary-only root must not expose reply TPS: %+v", got)
	}
}

func TestOutputTimingRootRejectsAmbiguousOrForeignEvidence(t *testing.T) {
	cases := map[string]func() []RequestAttempt{
		"unresolved reply without output": func() []RequestAttempt {
			pending := fixtureOutputAttempt("pending", "reply")
			pending.OutputTiming.HasOutput, pending.OutputTiming.Terminal = false, false
			return []RequestAttempt{pending, fixtureOutputAttempt("reply", "reply")}
		},
		"missing identity even without usage": func() []RequestAttempt {
			pending := fixtureOutputAttempt("pending", "reply")
			pending.OutputTiming = nil
			return []RequestAttempt{pending, fixtureOutputAttempt("reply", "reply")}
		},
		"successful other reply lifecycle without output": func() []RequestAttempt {
			other := fixtureOutputAttempt("other", "reply")
			other.OutputTiming.HasOutput = false
			other.OutputTiming.LifecycleID = "other/lifetime"
			return []RequestAttempt{other, fixtureOutputAttempt("reply", "reply")}
		},
		"atomic unmeasurable reply still counts as output": func() []RequestAttempt {
			other := fixtureOutputAttempt("atomic", "reply")
			other.OutputTiming.Status = aistudio.OutputTimingUnavailable
			return []RequestAttempt{other, fixtureOutputAttempt("reply", "reply")}
		},
		"multiple reply outputs": func() []RequestAttempt {
			return []RequestAttempt{fixtureOutputAttempt("a", "reply"), fixtureOutputAttempt("b", "reply")}
		},
		"missing evidence with known output": func() []RequestAttempt {
			prior := fixtureOutputAttempt("prior", "reply")
			prior.OutputTiming = nil
			prior.Usage = &aistudio.Usage{OutputTokens: 300}
			return []RequestAttempt{prior, fixtureOutputAttempt("reply", "reply")}
		},
		"foreign attempt": func() []RequestAttempt {
			a := fixtureOutputAttempt("reply", "reply")
			a.OutputTiming.AttemptID = "foreign"
			return []RequestAttempt{a}
		},
		"foreign generation": func() []RequestAttempt {
			a := fixtureOutputAttempt("reply", "reply")
			a.OutputTiming.RequestID = "foreign"
			return []RequestAttempt{a}
		},
		"unknown purpose": func() []RequestAttempt {
			return []RequestAttempt{fixtureOutputAttempt("reply", "other")}
		},
		"failed attempt": func() []RequestAttempt {
			a := fixtureOutputAttempt("reply", "reply")
			a.Error = "fixture failure"
			return []RequestAttempt{a}
		},
		"not sent": func() []RequestAttempt {
			a := fixtureOutputAttempt("reply", "reply")
			*a.Sent = false
			return []RequestAttempt{a}
		},
		"no response": func() []RequestAttempt {
			a := fixtureOutputAttempt("reply", "reply")
			*a.ResponseReceived = false
			return []RequestAttempt{a}
		},
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			if got := outputTimingFromAttempts(rows()); got != nil && got.TPS() != nil {
				t.Fatalf("invalid attribution produced TPS: %+v", got)
			}
		})
	}
}

func TestOutputTimingCloneAndSnapshotIsolation(t *testing.T) {
	attempt := fixtureOutputAttempt("reply", "reply")
	cloned := CloneRequestAttempts([]RequestAttempt{attempt})
	cloned[0].OutputTiming.DurationMS = 1
	if attempt.OutputTiming.DurationMS != 8000 {
		t.Fatal("attempt clone aliases timing")
	}
	metadata := RequestLedgerMetadata{OutputTiming: attempt.OutputTiming}
	clone := metadata.Clone()
	clone.OutputTiming.DurationMS = 1
	if metadata.OutputTiming.DurationMS != 8000 {
		t.Fatal("metadata clone aliases timing")
	}

	root := &accessLogMetadata{requestID: "fixture/root"}
	ctx := context.WithValue(context.Background(), accessLogContextKey{}, root)
	native := aistudio.GenerationAttempt{
		ID: attempt.ID, RequestID: attempt.RequestID, Revision: 1,
		Sent: true, ResponseReceived: true, StatusCode: 200,
		OutputTiming: attempt.OutputTiming,
	}
	AddAccessLogGenerationAttempt(ctx, native, "fixture-account")
	native.OutputTiming.DurationMS = 1
	snapshot := root.snapshot()
	if snapshot.ledger.OutputTiming == nil || *snapshot.ledger.OutputTiming.TPS() != 65 {
		t.Fatalf("access snapshot lost detached timing: %+v", snapshot.ledger.OutputTiming)
	}
	snapshot.ledger.OutputTiming.DurationMS = 2
	if *root.snapshot().ledger.OutputTiming.TPS() != 65 {
		t.Fatal("async snapshot mutates live state")
	}
}

func TestOutputTimingRecordNumericOnlyAndOutcome(t *testing.T) {
	a := fixtureOutputAttempt("reply", "reply")
	record := UsageRecord{
		RequestLedgerMetadata: RequestLedgerMetadata{OutputTiming: a.OutputTiming},
		Status:                200, State: "completed", Attempts: []RequestAttempt{a},
	}
	record.OutputTPS = record.ObservedOutputTPS()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["output_tps"] != float64(65) || decoded["output_timing"] != nil {
		t.Fatalf("public record must expose only numeric TPS: %s", data)
	}
	attemptJSON := decoded["attempts"].([]any)[0].(map[string]any)
	if _, exists := attemptJSON["output_timing"]; exists {
		t.Fatal("timing must not silently expand public attempts/CSV")
	}
	for _, state := range []string{"failed", "cancelled", "running", "queued", "unknown", ""} {
		record.State = state
		if record.ObservedOutputTPS() != nil {
			t.Fatalf("state %q must not expose final numeric TPS", state)
		}
	}
	record.State, record.Status = "completed", 502
	if record.ObservedOutputTPS() != nil {
		t.Fatal("HTTP failure must not expose numeric TPS")
	}
	record.Status, record.Error = 200, "fixture final error"
	if record.ObservedOutputTPS() != nil {
		t.Fatal("late error must suppress TPS")
	}
}
