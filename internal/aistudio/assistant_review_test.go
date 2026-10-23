package aistudio

import (
	"bytes"
	"errors"
	"testing"
)

func TestAssistantReviewPendingLaterStepCannotReuseEarlierCompletion(t *testing.T) {
	for _, modelStep := range []bool{false, true} {
		t.Run(map[bool]string{false: "control-step", true: "model-step"}[modelStep], func(t *testing.T) {
			first := assistantStep("model", 1, 1, nil, assistantPart("first", false))
			next := assistantStep("model", 2, 2, nil, assistantPart("partial", false))
			if !modelStep {
				next[0] = nil
			}
			events, err := assistantCollect(t, bytes.NewReader(assistantStreamBytes(assistantMessage(first, next))))
			if err == nil || assistantFinish(events) != nil {
				t.Fatalf("pending step became stop: %v %+v", err, events)
			}
		})
	}
}

func TestAssistantReviewUnknownOrFailedStepStatusIsNotSuccess(t *testing.T) {
	for _, status := range []int64{0, 3, 4, 99} {
		step := assistantStep("model", 1, status, nil, assistantPart("fixture", false))
		events, err := assistantCollect(t, bytes.NewReader(assistantStreamBytes(assistantMessage(step))))
		if err == nil || assistantFinish(events) != nil {
			t.Fatalf("status %d became success", status)
		}
	}
}

func TestAssistantReviewUnmappedOutputPartIsNotSilentlyDiscarded(t *testing.T) {
	for _, slot := range []int{2, 5, 7, 8, 10} {
		part := make([]any, 15)
		part[slot] = []any{"fixture-only"}
		stream := assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, nil, part)))
		events, err := assistantCollect(t, bytes.NewReader(stream))
		var evidence *ProtocolEvidenceError
		if !errors.As(err, &evidence) || assistantFinish(events) != nil {
			t.Fatalf("unmapped Part[%d] was dropped: %v %+v", slot, err, events)
		}
	}
}

func TestAssistantReviewAcceptedFailuresUpdateOriginalAttempt(t *testing.T) {
	for _, missingID := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream-open", true: "task-id"}[missingID], func(t *testing.T) {
			fixture := newAssistantFixture(t)
			if missingID {
				fixture.initBody = []byte(`[]`)
			} else {
				fixture.streamErr = errors.New("fixture read stream unavailable")
			}
			if _, _, err := fixture.client.sendAssistant(fixture.ctx, assistantTestRequest(), assistantTestEntry()); err == nil {
				t.Fatal("failure lost")
			}
			_, sends, attempts := fixture.snapshot()
			if len(sends) != 1 || len(attempts) != 1 {
				t.Fatal("wrong generation count")
			}
			for _, attempt := range attempts {
				if attempt.Stage != "failed" || attempt.Usage != nil || attempt.UsageSource != "unknown" {
					t.Fatalf("accepted failure not attributed: %+v", attempt)
				}
			}
		})
	}
}

func TestAssistantReviewPartialFailurePreservesKnownUsage(t *testing.T) {
	for _, brokenEnvelope := range []bool{false, true} {
		t.Run(map[bool]string{false: "later-step", true: "envelope"}[brokenEnvelope], func(t *testing.T) {
			complete := assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("known", false))
			pending := assistantStep("model", 2, 2, nil, assistantPart("partial", false))
			stream := assistantStreamBytes(assistantMessage(complete, pending))
			if brokenEnvelope {
				stream = stream[:len(stream)-1]
			}
			events, err := assistantCollect(t, bytes.NewReader(stream))
			if err == nil || assistantFinish(events) != nil {
				t.Fatal("partial response became success")
			}
			usage := assistantUsage(events)
			if usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
				t.Fatalf("known partial usage lost: %+v", usage)
			}
		})
	}
}

func TestAssistantReviewSnapshotsReplaceUsageRatherThanDoubleCount(t *testing.T) {
	first := assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("one", false))
	revised := assistantStep("model", 1, 1, []any{"12", "6", "2"}, assistantPart("one", false))
	second := assistantStep("model", 2, 1, []any{"3", "4", "0"}, assistantPart("two", false))
	events, err := assistantCollect(t, bytes.NewReader(assistantStreamBytes(assistantMessage(first), assistantMessage(revised, second))))
	if err != nil || assistantFinish(events) == nil {
		t.Fatal(err)
	}
	var usage *Usage
	for _, event := range events {
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if usage == nil || usage.TotalTokens != 27 || usage.InputTokens != 17 || usage.OutputTokens != 10 {
		t.Fatalf("snapshots double-counted: %+v", usage)
	}
}

func TestAssistantReviewLeasePinCannotBeBypassed(t *testing.T) {
	_, _, lease := assistantCoreFixture(t)
	selection := AccountSelection{ModelID: "gemini-4-argon", Method: "generateContent", Channel: ChannelAssistant, StrictChannel: true}
	if err := validateLeaseSelection(lease, selection); err != nil {
		t.Fatal(err)
	}
	selection.StrictChannel = false
	if err := validateLeaseSelection(lease, selection); err == nil {
		t.Fatal("unstrict assistant generation admitted")
	}
	selection.Channel = ChannelBuild
	selection.StrictChannel = true
	if err := validateLeaseSelection(lease, selection); err == nil {
		t.Fatal("Build borrowed assistant lease")
	}
	if err := validateLeaseSelection(lease, AccountSelection{AccountID: lease.Account().ID}); err != nil {
		t.Fatal("auxiliary request lost own lease", err)
	}
}
