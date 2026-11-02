package aistudio

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type outputFixtureChunk struct {
	body   string
	second int
}
type outputFixtureReader struct {
	chunks               []outputFixtureChunk
	index, offset, reads int
	now                  time.Time
	trusted              bool
}

func (reader *outputFixtureReader) OutputReadSourceKnown() bool { return reader.trusted }
func (reader *outputFixtureReader) Read(destination []byte) (int, error) {
	if reader.index == len(reader.chunks) {
		return 0, io.EOF
	}
	chunk := reader.chunks[reader.index]
	reader.now = time.Unix(1000+int64(chunk.second), 0)
	count := copy(destination, chunk.body[reader.offset:])
	reader.offset += count
	reader.reads++
	if reader.offset == len(chunk.body) {
		reader.index++
		reader.offset = 0
	}
	return count, nil
}

func outputPGFrame(text string, count *int64, finish bool, thought bool) string {
	part := make([]any, 13)
	part[1], part[12] = text, thought
	var end any
	if finish {
		end = 1
	}
	candidate := []any{[]any{[]any{part}, "model"}, end}
	var usage any
	if count != nil {
		usage = []any{1000, *count, 1000 + *count + 77, 0, nil, nil, nil, nil, nil, 77}
	}
	encoded, _ := json.Marshal([]any{[]any{candidate}, nil, usage})
	return string(encoded)
}

func outputCount(value int64) *int64 { return &value }

func decodeOutputFixture(t *testing.T, chunks []outputFixtureChunk, trusted bool) (*OutputTiming, []Event, error, int) {
	t.Helper()
	reader := &outputFixtureReader{chunks: chunks, trusted: trusted}
	window := newOutputWindow(contextWithOutputLifetime(context.Background()), GenerationAttempt{ID: "fixture/physical", RequestID: "fixture/request"})
	observation := &outputDecoderTiming{window: window, clock: func() time.Time { return reader.now }}
	decoder := NewFrameDecoder()
	decoder.output = observation
	var events []Event
	err := DecodeGenerateStream(reader, decoder, func(event Event) error { events = append(events, event); return nil })
	if err == nil {
		err = decoder.End()
	}
	window.finish(err == nil && observation.complete && decoder.finished)
	return window.snapshot(), events, err, reader.reads
}

func TestOutputWindowRealDecoderSparseOffsetAndTail(t *testing.T) {
	first := strings.ReplaceAll(outputPGFrame("first", outputCount(100), false, false), "null", "")
	last := strings.ReplaceAll(outputPGFrame("last", outputCount(620), true, false), "null", "")
	got, events, err, reads := decodeOutputFixture(t, []outputFixtureChunk{{"[[" + first + ",", 80}, {last, 88}, {"]]", 150}}, true)
	if err != nil || got.TPS() == nil || *got.TPS() != 65 || got.ProgressBatches != 2 || got.DurationMS != 8000 || reads != 3 {
		t.Fatalf("sparse/read-ahead/tail: evidence=%+v error=%v reads=%d", got, err, reads)
	}
	var texts []string
	for _, event := range events {
		if event.Kind == EventText {
			texts = append(texts, event.Text)
		}
	}
	if !reflect.DeepEqual(texts, []string{"first", "last"}) {
		t.Fatalf("changed text delivery: %v", texts)
	}
}

func TestOutputWindowSingleReadManyFramesDoesNotMeasure(t *testing.T) {
	body := "[[" + outputPGFrame("first", outputCount(100), false, false) + "," + outputPGFrame("last", outputCount(620), true, false) + "]]"
	got, _, err, reads := decodeOutputFixture(t, []outputFixtureChunk{{body, 0}}, true)
	if err != nil || got.TPS() != nil || got.ProgressBatches != 1 || reads != 1 || !got.HasOutput || !got.Complete {
		t.Fatalf("one real read became a window: %+v %v reads=%d", got, err, reads)
	}
}

func TestOutputWindowReadAheadDoesNotBorrowLatestBatchClock(t *testing.T) {
	first := outputPGFrame("first", outputCount(100), false, false)
	second := outputPGFrame("second", outputCount(300), false, false)
	last := outputPGFrame("last", outputCount(620), true, false)
	got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{{"[[" + first + "," + second + ",", 80}, {last + "]]", 88}}, true)
	if err != nil || got.TPS() == nil || *got.TPS() != 40 || got.StartOutputTokens != 300 {
		t.Fatalf("first batch must exclude ALL 300 tokens, not only first frame: %+v %v", got, err)
	}
}

func TestOutputWindowUnsupportedBufferedSourceDoesNotMeasure(t *testing.T) {
	got, events, err, _ := decodeOutputFixture(t, []outputFixtureChunk{{"[[" + outputPGFrame("a", outputCount(100), false, false) + ",", 0}, {outputPGFrame("b", outputCount(620), true, false) + "]]", 8}}, false)
	if err != nil || got.TPS() != nil || got.Reason != "reader_source_unqualified" || len(events) == 0 {
		t.Fatalf("unknown source must not change decode or invent timing: %+v %v", got, err)
	}
}

func TestOutputWindowDoesNotReuseCachedUsageOrLateMetadata(t *testing.T) {
	first := outputPGFrame("first", outputCount(100), false, false)
	noCount := outputPGFrame("uncounted", nil, false, false)
	late := outputPGFrame("", outputCount(620), true, false)
	got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{{"[[" + first + ",", 0}, {noCount + ",", 4}, {late + "]]", 8}}, true)
	if err != nil || got.TPS() != nil {
		t.Fatalf("cached or late count borrowed into text: %+v %v", got, err)
	}
}

func TestOutputWindowCrossReadFrameResetsRatherThanUsesJSONTail(t *testing.T) {
	first := outputPGFrame("first", outputCount(100), false, false)
	crossed := outputPGFrame("crossed", outputCount(200), false, false)
	start := outputPGFrame("start", outputCount(300), false, false)
	last := outputPGFrame("last", outputCount(620), true, false)
	cut := len(crossed) / 2
	got, _, err, _ := decodeOutputFixture(t, []outputFixtureChunk{
		{"[[" + first + ",", 0}, {crossed[:cut], 2}, {crossed[cut:] + "," + start + ",", 4}, {last + "]]", 8},
	}, true)
	if err != nil || got.TPS() == nil || *got.TPS() != 80 || got.StartOutputTokens != 300 || got.DurationMS != 4000 {
		t.Fatalf("must use only last aligned window, not crossed frame's closing bytes: %+v %v", got, err)
	}
}

func TestOutputWindowReasoningInterruptStartsNewWindow(t *testing.T) {
	frames := []outputFixtureChunk{
		{"[[" + outputPGFrame("old reply", outputCount(100), false, false) + ",", 0},
		{outputPGFrame("thought summary", outputCount(100), false, true) + ",", 4},
		{outputPGFrame("new reply", outputCount(200), false, false) + ",", 6},
		{outputPGFrame("last", outputCount(620), true, false) + "]]", 10},
	}
	got, _, err, _ := decodeOutputFixture(t, frames, true)
	if err != nil || got.TPS() == nil || *got.TPS() != 105 || got.StartOutputTokens != 200 || got.DurationMS != 4000 {
		t.Fatalf("reasoning must not enter output window or clip a fast result: %+v %v", got, err)
	}
}

func TestOutputWindowInvalidCoverageNeverMeasures(t *testing.T) {
	first := outputPGFrame("first", outputCount(100), false, false)
	for name, chunks := range map[string][]outputFixtureChunk{
		"missing finish":    {{"[[" + first + ",", 0}, {outputPGFrame("last", outputCount(620), false, false) + "]]", 8}},
		"truncated root":    {{"[[" + first + ",", 0}, {outputPGFrame("last", outputCount(620), true, false), 8}},
		"counter reset":     {{"[[" + first + ",", 0}, {outputPGFrame("last", outputCount(20), true, false) + "]]", 8}},
		"same clock":        {{"[[" + first + ",", 0}, {outputPGFrame("last", outputCount(620), true, false) + "]]", 0}},
		"duplicate counter": {{"[[" + first + ",", 0}, {outputPGFrame("last", outputCount(100), true, false) + "]]", 8}},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, _, _ := decodeOutputFixture(t, chunks, true)
			if got.TPS() != nil {
				t.Fatalf("invalid evidence measured: %+v", got)
			}
		})
	}
}

func TestOutputWindowAtomicToolMediaAndCancellation(t *testing.T) {
	for _, kind := range []EventKind{EventToolCall, EventExecutableCode, EventCodeExecutionResult, EventMedia} {
		t.Run(string(kind), func(t *testing.T) {
			window := newOutputWindow(context.Background(), GenerationAttempt{ID: "fixture"})
			window.observeFrame([]Event{{Kind: EventText, Text: "first"}}, 100, true, nil, outputReadBatch{1, time.Unix(0, 0)}, true, nil)
			event := Event{Kind: kind, ToolCall: &FunctionCall{}, ExecutableCode: &ExecutableCode{}, CodeExecutionResult: &CodeExecutionResult{}, Media: &Media{}}
			window.observeFrame([]Event{event}, 620, true, nil, outputReadBatch{2, time.Unix(8, 0)}, true, nil)
			window.finish(true)
			if got := window.snapshot(); got.TPS() != nil || !got.HasOutput {
				t.Fatalf("atomic unsupported output: %+v", got)
			}
		})
	}
	window := newOutputWindow(context.Background(), GenerationAttempt{ID: "fixture"})
	window.observeFrame([]Event{{Kind: EventText, Text: "a"}}, 100, true, nil, outputReadBatch{1, time.Unix(0, 0)}, true, nil)
	window.observeFrame([]Event{{Kind: EventText, Text: "b"}}, 620, true, nil, outputReadBatch{2, time.Unix(8, 0)}, true, nil)
	window.finish(false)
	window.finish(true) // a late successful callback cannot repair a canceled observation
	if got := window.snapshot(); got.TPS() != nil || got.Complete || !got.Terminal {
		t.Fatalf("cancel overwritten: %+v", got)
	}
}
