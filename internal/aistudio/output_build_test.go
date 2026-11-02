package aistudio

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func outputBuildFrame(text string, count *int64, finish bool, derive bool) string {
	candidate := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
	if finish {
		candidate["finishReason"] = "STOP"
	}
	response := map[string]any{"candidates": []any{candidate}}
	if count != nil {
		response["usageMetadata"] = map[string]any{"candidatesTokenCount": *count}
	}
	if derive {
		response["usageMetadata"] = map[string]any{"promptTokenCount": 1000, "thoughtsTokenCount": 77, "totalTokenCount": 1697}
	}
	inner, _ := json.Marshal(response)
	outer, _ := json.Marshal([]any{string(inner)})
	return string(outer)
}

func decodeBuildOutputFixture(t *testing.T, chunks []outputFixtureChunk) (*OutputTiming, error) {
	t.Helper()
	reader := &outputFixtureReader{chunks: chunks, trusted: true}
	window := newOutputWindow(context.Background(), GenerationAttempt{ID: "fixture/build"})
	observation := &outputDecoderTiming{window: window, clock: func() time.Time { return reader.now }}
	decoder := &BuildStreamDecoder{output: observation}
	err := DecodeBuildStream(reader, decoder, func(Event) error { return nil })
	window.finish(err == nil && observation.complete && decoder.finished)
	return window.snapshot(), err
}

func TestOutputWindowBuildFreshCounterWithoutTotalOrReasoning(t *testing.T) {
	first, last := outputBuildFrame("a", outputCount(100), false, false), outputBuildFrame("b", outputCount(620), true, false)
	got, err := decodeBuildOutputFixture(t, []outputFixtureChunk{{"[[" + first + ",", 80}, {last, 88}, {"]]", 150}})
	if err != nil || got.TPS() == nil || *got.TPS() != 65 || got.DurationMS != 8000 {
		t.Fatalf("build fresh O must not require total or reasoning: %+v %v", got, err)
	}
}

func TestOutputWindowBuildDerivedOrCachedOutputIsNotFresh(t *testing.T) {
	for _, derive := range []bool{true, false} {
		first, last := outputBuildFrame("a", outputCount(100), false, false), outputBuildFrame("b", nil, true, derive)
		got, err := decodeBuildOutputFixture(t, []outputFixtureChunk{{"[[" + first + ",", 80}, {last + "]]", 88}})
		if err != nil || got.TPS() != nil {
			t.Fatalf("derived/cached O became fresh counter: %+v %v", got, err)
		}
	}
}

func TestOutputWindowBuildOneReadAndBadTrailer(t *testing.T) {
	first, last := outputBuildFrame("a", outputCount(100), false, false), outputBuildFrame("b", outputCount(620), true, false)
	got, err := decodeBuildOutputFixture(t, []outputFixtureChunk{{"[[" + first + "," + last + "]]", 80}})
	if err != nil || got.TPS() != nil {
		t.Fatalf("single build source batch became timing: %+v %v", got, err)
	}
	got, err = decodeBuildOutputFixture(t, []outputFixtureChunk{{"[[" + first + ",", 80}, {last + "]", 88}, {",[13,\"fixture failure\"]]", 150}})
	if err == nil || got.TPS() != nil || got.Complete {
		t.Fatalf("bad trailer became complete measured window: %+v %v", got, err)
	}
}
