package aistudio

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func outputUsageOnly(count int64) string {
	encoded, _ := json.Marshal([]any{nil, nil, []any{1000, count, 1000 + count}})
	return string(encoded)
}

func TestOutputWindowMissingMiddleCounterIsNotARequiredBoundary(t *testing.T) {
	got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{
		{"[[" + outputPGFrame("a", outputCount(100), false, false) + ",", 80},
		{outputPGFrame("middle", nil, false, false) + ",", 84},
		{outputPGFrame("b", outputCount(620), true, false) + "]]", 88},
	}, true)
	if err != nil || got.TPS() == nil || *got.TPS() != 65 {
		t.Fatalf("pure middle text need not carry an endpoint count: %+v %v", got, err)
	}
}

func TestOutputWindowSameBatchUsageCoverageOrder(t *testing.T) {
	for _, after := range []bool{true, false} {
		t.Run(map[bool]string{true: "text_then_usage", false: "usage_then_text"}[after], func(t *testing.T) {
			first, last := outputPGFrame("a", nil, false, false), outputPGFrame("b", nil, true, false)
			if after {
				first += "," + outputUsageOnly(100)
				last += "," + outputUsageOnly(620)
			} else {
				first = outputUsageOnly(100) + "," + first
				last = outputUsageOnly(620) + "," + last
			}
			got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{{"[[" + first + ",", 80}, {last + "]]", 88}}, true)
			if err != nil {
				t.Fatal(err)
			}
			if after {
				if got.TPS() == nil || *got.TPS() != 65 {
					t.Fatalf("same-read post-text snapshot did not cover batch: %+v", got)
				}
			} else if got.TPS() != nil {
				t.Fatalf("a pre-text count was reused after text: %+v", got)
			}
		})
	}
}

func TestOutputWindowEventOrderAndTranscriptBreakContinuity(t *testing.T) {
	for name, events := range map[string][]Event{
		"text then thought": {{Kind: EventText, Text: "before"}, {Kind: EventReasoning, Text: "thinking"}},
		"audio transcript":  {{Kind: EventText, Text: "transcript", Transcript: &TranscriptMetadata{}}},
	} {
		t.Run(name, func(t *testing.T) {
			window := newOutputWindow(context.Background(), GenerationAttempt{ID: "fixture"})
			window.observeFrame([]Event{{Kind: EventText, Text: "a"}}, 100, true, nil, outputReadBatch{1, time.Unix(0, 0)}, true, nil)
			window.observeFrame(events, 200, true, nil, outputReadBatch{2, time.Unix(4, 0)}, true, nil)
			window.observeFrame([]Event{{Kind: EventText, Text: "b"}}, 620, true, nil, outputReadBatch{3, time.Unix(8, 0)}, true, nil)
			window.finish(true)
			if got := window.snapshot(); got.TPS() != nil || got.ProgressBatches != 1 {
				t.Fatalf("pre-interruption baseline survived: %+v", got)
			}
		})
	}
}

func TestOutputWindowUsageOnlyCounterRegressionSurvivesReset(t *testing.T) {
	window := newOutputWindow(context.Background(), GenerationAttempt{ID: "fixture"})
	window.observeFrame([]Event{{Kind: EventText, Text: "a"}}, 150, true, nil, outputReadBatch{1, time.Unix(0, 0)}, true, nil)
	window.observeFrame(nil, 120, true, nil, outputReadBatch{2, time.Unix(2, 0)}, true, nil)
	window.observeFrame([]Event{{Kind: EventReasoning, Text: "phase"}}, 180, true, nil, outputReadBatch{3, time.Unix(4, 0)}, true, nil)
	window.observeFrame([]Event{{Kind: EventText, Text: "b"}}, 200, true, nil, outputReadBatch{4, time.Unix(6, 0)}, true, nil)
	window.observeFrame([]Event{{Kind: EventText, Text: "c"}}, 300, true, nil, outputReadBatch{5, time.Unix(8, 0)}, true, nil)
	window.finish(true)
	if got := window.snapshot(); got.TPS() != nil || got.Reason != "output_counter_reset" {
		t.Fatalf("phase reset washed away counter regression: %+v", got)
	}
}

func TestOutputReadMapBoundsMemoryWithoutPoisoningLaterFrames(t *testing.T) {
	window := newOutputWindow(context.Background(), GenerationAttempt{ID: "fixture"})
	mapping := &outputDecoderTiming{window: window, reader: &outputBatchReader{}}
	for i := 1; i <= maxOutputReadSpans*3; i++ {
		mapping.normalized(1, outputReadBatch{uint64(i), time.Unix(int64(i), 0)})
		if len(mapping.spans) > maxOutputReadSpans {
			t.Fatal("unbounded metadata")
		}
	}
	mapping.frameRange(mapping.offset, int(mapping.offset))
	if mapping.paired {
		t.Fatal("huge multi-read frame became an endpoint")
	}
	for i := 0; i < 2; i++ {
		mapping.normalized(10, outputReadBatch{uint64(10000 + i), time.Unix(int64(10000+i), 0)})
		mapping.frameRange(mapping.offset, 10)
		if !mapping.paired {
			t.Fatal("discarded history poisoned a later complete frame")
		}
		mapping.observe([]Event{{Kind: EventText, Text: "later"}}, int64(100+i*70), true, nil, nil)
	}
	window.finish(true)
	if got := window.snapshot(); got.TPS() == nil || *got.TPS() != 70 {
		t.Fatalf("later bounded window unavailable: %+v", got)
	}
}

func TestOutputWindowUnexplainedUsageGrowthIsNotBorrowedIntoLaterText(t *testing.T) {
	got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{
		{"[[" + outputPGFrame("a", outputCount(100), false, false) + ",", 80},
		{outputUsageOnly(300) + ",", 84},
		{outputPGFrame("b", outputCount(620), true, false) + "]]", 88},
	}, true)
	if err != nil || got.TPS() != nil {
		t.Fatalf("unexplained counter growth entered later window: %+v %v", got, err)
	}
}

func TestOutputWindowUsageCanCoverPreviouslyUncountedTextWithoutBecomingAnEndpoint(t *testing.T) {
	got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{
		{"[[" + outputPGFrame("a", outputCount(100), false, false) + ",", 80},
		{outputPGFrame("middle", nil, false, false) + ",", 83},
		{outputUsageOnly(300) + ",", 84},
		{outputPGFrame("b", outputCount(620), true, false) + "]]", 88},
	}, true)
	if err != nil || got.TPS() == nil || *got.TPS() != 65 || got.ProgressBatches != 2 {
		t.Fatalf("delayed counter must not move text time: %+v %v", got, err)
	}
}

func TestOutputLifetimeDoesNotReuseRequestIDOrPurpose(t *testing.T) {
	summary := newOutputWindow(contextWithOutputLifetime(ContextWithGenerationPurpose(context.Background(), "compaction")), GenerationAttempt{ID: "summary", RequestID: "reused"})
	reply := newOutputWindow(contextWithOutputLifetime(context.Background()), GenerationAttempt{ID: "reply", RequestID: "reused"})
	a, b := summary.snapshot(), reply.snapshot()
	if a.LifecycleID == b.LifecycleID || a.Purpose != "compaction" || b.Purpose != "reply" {
		t.Fatalf("reused generation ID borrowed execution identity: %+v %+v", a, b)
	}
}
