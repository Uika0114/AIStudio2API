package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// Raw fixtures use the native protocols, not projected/log Usage defaults.
func generationFrame(build, atomic, output, finish bool, usage map[string]any) []byte {
	if build {
		response := map[string]any{}
		if output || finish {
			candidate := map[string]any{}
			if output {
				part := map[string]any{"text": "fixture"}
				if atomic {
					part = map[string]any{"functionCall": map[string]any{"name": "emit", "args": map[string]any{"content": "fixture"}}}
				}
				candidate["content"] = map[string]any{"parts": []any{part}}
			}
			if finish {
				candidate["finishReason"] = "STOP"
			}
			response["candidates"] = []any{candidate}
		}
		if usage != nil {
			response["usageMetadata"] = usage
		}
		raw, _ := json.Marshal(response)
		return raw
	}
	var candidates any
	if output || finish {
		candidate := make([]any, 2)
		if output {
			part := make([]any, 11)
			part[1] = "fixture"
			if atomic {
				part[1] = nil
				part[10] = []any{"emit", []any{[]any{[]any{"content", []any{nil, nil, "fixture"}}}}}
			}
			candidate[0] = []any{[]any{part}, "model"}
		}
		if finish {
			candidate[1] = 1
		}
		candidates = []any{candidate}
	}
	var counts any
	if usage != nil {
		values := make([]any, 10)
		for index, name := range map[int]string{0: "promptTokenCount", 1: "candidatesTokenCount", 2: "totalTokenCount", 3: "cachedContentTokenCount", 7: "toolUsePromptTokenCount", 9: "thoughtsTokenCount"} {
			values[index] = usage[name]
		}
		counts = values
	}
	raw, _ := json.Marshal([]any{candidates, nil, counts})
	return raw
}
func generationUsage() map[string]any {
	return map[string]any{"promptTokenCount": 195, "candidatesTokenCount": 420, "thoughtsTokenCount": 100, "totalTokenCount": 715}
}
func generationFixture(build bool) (*generationTiming, func([]byte) ([]Event, error), *time.Time) {
	now := time.Unix(1000, 0)
	timing := newGenerationTiming(context.Background(), GenerationAttempt{ID: "fixture/attempt", RequestID: "reply"})
	timing.clock = func() time.Time { return now }
	observation := &generationDecoderTiming{timing: timing}
	var decode func([]byte) ([]Event, error)
	if build {
		d := &BuildStreamDecoder{output: observation}
		decode = d.Decode
	} else {
		d := &FrameDecoder{output: observation}
		decode = func(raw []byte) ([]Event, error) { return d.Decode(raw) }
	}
	timing.markSend()
	now = now.Add(8 * time.Second)
	return timing, decode, &now
}
func assertGeneration(t *testing.T, got *OutputTiming, want float64) {
	t.Helper()
	if got.TPS() == nil || *got.TPS() != want {
		t.Fatalf("TPS=%v evidence=%+v want %v", got.TPS(), got, want)
	}
}

// This exact atomic positive failed on 75cd069 with non_output_phase/nil.
func TestGenerationAverageAtomicPositive(t *testing.T) {
	for _, build := range []bool{false, true} {
		t.Run(map[bool]string{false: "PG", true: "Build"}[build], func(t *testing.T) {
			for _, atomic := range []bool{false, true} {
				timing, decode, _ := generationFixture(build)
				events, err := decode(generationFrame(build, atomic, true, true, generationUsage()))
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Kind == EventReasoning {
						t.Fatal("fixture must have no thought summary")
					}
				}
				timing.finish(true)
				assertGeneration(t, timing.snapshot(), 65)
			}
		})
	}
}

func TestGenerationAverageRawCountPresence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   *float64
		source string
	}{
		{"independent", func(m map[string]any) { delete(m, "totalTokenCount"); delete(m, "promptTokenCount") }, number(65), "output_plus_reasoning"},
		{"explicit_zero", func(m map[string]any) { m["thoughtsTokenCount"] = 0; m["totalTokenCount"] = 615 }, number(52.5), "output_plus_reasoning"},
		{"missing_R_derived", func(m map[string]any) { delete(m, "thoughtsTokenCount") }, number(65), "total_minus_input_tool"},
		{"missing_O_derived", func(m map[string]any) { delete(m, "candidatesTokenCount") }, number(65), "total_minus_input_tool"},
		{"tool_input_and_cache", func(m map[string]any) {
			delete(m, "thoughtsTokenCount")
			m["cachedContentTokenCount"] = 100
			m["toolUsePromptTokenCount"] = 45
			m["totalTokenCount"] = 760
		}, number(65), "total_minus_input_tool"},
		{"missing_R_not_zero", func(m map[string]any) { delete(m, "thoughtsTokenCount"); delete(m, "totalTokenCount") }, nil, ""},
		{"total_without_input", func(m map[string]any) { delete(m, "thoughtsTokenCount"); delete(m, "promptTokenCount") }, nil, ""},
		{"inconsistent", func(m map[string]any) { m["totalTokenCount"] = 716 }, nil, ""},
		{"negative_output", func(m map[string]any) { m["candidatesTokenCount"] = -1 }, nil, ""},
		{"negative_R", func(m map[string]any) { m["thoughtsTokenCount"] = -1 }, nil, ""},
		{"negative_input", func(m map[string]any) { m["promptTokenCount"] = -1 }, nil, ""},
		{"negative_tool", func(m map[string]any) { m["toolUsePromptTokenCount"] = -1 }, nil, ""},
		{"sum_overflow", func(m map[string]any) { m["candidatesTokenCount"] = int64(math.MaxInt64); delete(m, "totalTokenCount") }, nil, ""},
		{"wire_overflow", func(m map[string]any) { m["candidatesTokenCount"] = json.Number("9223372036854775808") }, nil, ""},
		{"derived_negative", func(m map[string]any) { delete(m, "thoughtsTokenCount"); m["totalTokenCount"] = 1 }, nil, ""},
		{"derived_below_output", func(m map[string]any) { delete(m, "thoughtsTokenCount"); m["totalTokenCount"] = 200 }, nil, ""},
		{"zero_generated", func(m map[string]any) {
			m["thoughtsTokenCount"] = 0
			m["candidatesTokenCount"] = 0
			m["totalTokenCount"] = 195
		}, nil, ""},
	}
	for _, build := range []bool{false, true} {
		for _, test := range cases {
			t.Run(map[bool]string{false: "PG/", true: "Build/"}[build]+test.name, func(t *testing.T) {
				timing, decode, _ := generationFixture(build)
				usage := generationUsage()
				test.mutate(usage)
				_, err := decode(generationFrame(build, true, true, true, usage))
				timing.finish(err == nil)
				got := timing.snapshot()
				if test.want == nil {
					if got.TPS() != nil {
						t.Fatalf("unproven count produced value: %+v", got)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					assertGeneration(t, got, *test.want)
					if got.CountSource != test.source {
						t.Fatalf("source=%s", got.CountSource)
					}
				}
			})
		}
	}
}
func number(v float64) *float64 { return &v }

func TestGenerationAverageNoCachedSnapshotMixing(t *testing.T) {
	for _, build := range []bool{false, true} {
		for _, mode := range []string{"missing_usage", "missing_R", "late_complete"} {
			t.Run(map[bool]string{false: "PG/", true: "Build/"}[build]+mode, func(t *testing.T) {
				timing, decode, now := generationFixture(build)
				if _, err := decode(generationFrame(build, false, true, false, generationUsage())); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(time.Second)
				var usage map[string]any
				if mode == "missing_R" {
					usage = map[string]any{"candidatesTokenCount": 420}
				}
				if _, err := decode(generationFrame(build, true, true, true, usage)); err != nil {
					t.Fatal(err)
				}
				if mode == "late_complete" {
					*now = now.Add(30 * time.Second)
					if _, err := decode(generationFrame(build, false, false, false, generationUsage())); err != nil {
						t.Fatal(err)
					}
				}
				timing.finish(true)
				got := timing.snapshot()
				if mode == "late_complete" {
					assertGeneration(t, got, 520.0/9)
					if got.DurationMS != 9000 {
						t.Fatal(got)
					}
				} else if got.TPS() != nil {
					t.Fatalf("borrowed earlier usage/default R: %+v", got)
				}
			})
		}
	}
}

func TestGenerationAveragePhysicalSendAndTailClock(t *testing.T) {
	ctx := ContextWithGenerationAttemptObserver(context.Background(), func(GenerationAttempt) {})
	ctx, attempt := beginPhysicalAttempt(ctx, GenerateRequest{ID: "fixture"}, RPCRequest{Method: "GenerateContent"})
	now := time.Unix(1000, 0)
	attempt.output.clock = func() time.Time { return now }
	now = now.Add(80 * time.Second) // preparation is not included
	markPhysicalSend(ctx)
	now = now.Add(4 * time.Second)
	markPhysicalSend(ctx) // repeated marker cannot reset
	now = now.Add(4 * time.Second)
	d := &FrameDecoder{output: attempt.outputDecoder(), observe: attempt.observe}
	if _, err := d.Decode(generationFrame(false, true, true, true, nil)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(42 * time.Second)
	if _, err := d.Decode(generationFrame(false, false, false, false, generationUsage())); err != nil {
		t.Fatal(err)
	}
	// Repeated terminal and final validation/logging do not stretch the interval.
	attempt.output.observe([]Event{{Kind: EventFinish}}, nil, nil)
	attempt.output.finish(true)
	attempt.update(func(*GenerationAttempt) {})
	got := attempt.output.snapshot()
	assertGeneration(t, got, 65)
	if got.DurationMS != 8000 {
		t.Fatalf("setup/tail included: %+v", got)
	}
	attempt.output.finish(false)
	if attempt.output.snapshot().TPS() != nil {
		t.Fatal("late protocol failure survived")
	}
}

type generationChunk struct {
	body   string
	second int
}
type generationReader struct {
	chunks        []generationChunk
	index, offset int
	now           *time.Time
}

func (r *generationReader) Read(dst []byte) (int, error) {
	if r.index == len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	*r.now = time.Unix(1000+int64(chunk.second), 0)
	n := copy(dst, chunk.body[r.offset:])
	r.offset += n
	if r.offset == len(chunk.body) {
		r.index++
		r.offset = 0
	}
	return n, nil
}
func generationProxyFrame(raw []byte) string {
	encoded, _ := json.Marshal([]any{string(raw)})
	return string(encoded)
}

func TestGenerationAverageNativeProtocolTail(t *testing.T) {
	for _, mode := range []string{"PG", "BuildStream", "BuildUnary"} {
		for _, broken := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/complete", true: "/broken"}[broken], func(t *testing.T) {
				now := time.Unix(1000, 0)
				timing := newGenerationTiming(context.Background(), GenerationAttempt{ID: "fixture"})
				timing.clock = func() time.Time { return now }
				timing.markSend()
				observation := &generationDecoderTiming{timing: timing}
				frame := generationFrame(mode != "PG", true, true, true, generationUsage())
				body, tail := "[["+string(frame), "]]"
				if mode != "PG" {
					body = "[[" + generationProxyFrame(frame)
				}
				if mode == "BuildUnary" {
					body = generationProxyFrame(frame)
					tail = " "
				}
				if broken {
					if mode == "BuildUnary" {
						tail = " invalid"
					} else {
						tail = "]"
					}
				}
				reader := &generationReader{chunks: []generationChunk{{body, 8}, {tail, 90}}, now: &now}
				var err error
				var finished bool
				emit := func(Event) error { return nil }
				if mode == "PG" {
					d := &FrameDecoder{output: observation}
					err = DecodeGenerateStream(reader, d, emit)
					if err == nil {
						err = d.End()
					}
					finished = d.finished
				} else {
					d := &BuildStreamDecoder{output: observation}
					if mode == "BuildUnary" {
						err = DecodeBuildUnary(reader, d, emit)
					} else {
						err = DecodeBuildStream(reader, d, emit)
					}
					finished = d.finished
				}
				timing.finish(err == nil && observation.complete && finished)
				got := timing.snapshot()
				if broken {
					if err == nil || got.TPS() != nil || got.Complete {
						t.Fatalf("broken root measured: %+v %v", got, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					assertGeneration(t, got, 65)
					if got.DurationMS != 8000 {
						t.Fatalf("tail extended native finish: %+v", got)
					}
				}
			})
		}
	}
}

func TestGenerationAverageLifetimeConcurrentSnapshotsAndAssistant(t *testing.T) {
	summary := newGenerationTiming(contextWithOutputLifetime(ContextWithGenerationPurpose(context.Background(), "compaction")), GenerationAttempt{ID: "summary", RequestID: "same"})
	reply := newGenerationTiming(contextWithOutputLifetime(context.Background()), GenerationAttempt{ID: "reply", RequestID: "same"})
	if summary.snapshot().LifecycleID == reply.snapshot().LifecycleID || summary.snapshot().Purpose != "compaction" || reply.snapshot().Purpose != "reply" {
		t.Fatal("mixed lifetime")
	}
	timing, decode, _ := generationFixture(false)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				snapshot := timing.snapshot()
				snapshot.Counts.Output = -1
			}
		}()
	}
	if _, err := decode(generationFrame(false, true, true, true, generationUsage())); err != nil {
		t.Fatal(err)
	}
	timing.finish(true)
	wg.Wait()
	assertGeneration(t, timing.snapshot(), 65)
	assistant := newGenerationTiming(context.Background(), GenerationAttempt{ID: "assistant", Method: assistantGenerateMethod})
	assistant.finish(true)
	if got := assistant.snapshot(); got.TPS() != nil || !strings.Contains(got.Reason, "assistant") {
		t.Fatalf("submit became generation clock: %+v", got)
	}
	timing.finish(false)
	if timing.snapshot().TPS() != nil {
		t.Fatal(errors.New("incomplete stream measured"))
	}
}

// Exact usage arrays retained by the sanitized 2026-11-02 one-evidence.json.
// Their old HTTP/browser time is deliberately not used as a new TPS clock.
func TestGenerationAverageSanitizedPGCounts(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int64
	}{
		{`[32,2,103,null,[[1,32]],null,[[1,2]],null,null,69]`, 71},
		{`[195,6,270,null,[[1,195]],null,[[1,6]],null,null,69]`, 75},
	} {
		counts := pgGenerationCounts([]byte(test.raw))
		generated, source, reason := counts.generated()
		if reason != "" || generated != test.want || source != "output_plus_reasoning" {
			t.Fatalf("real shape: %+v %d %s", counts, generated, reason)
		}
		counts.ReasoningKnown = false
		generated, source, reason = counts.generated()
		if reason != "" || generated != test.want || source != "total_minus_input_tool" {
			t.Fatalf("derived real shape: %+v %d %s", counts, generated, reason)
		}
	}
}

func TestGenerationAverageDurationAndLocalClockValidation(t *testing.T) {
	for _, seconds := range []int{-1, 0, 16} {
		timing, decode, now := generationFixture(false)
		*now = time.Unix(1000+int64(seconds), 0)
		if _, err := decode(generationFrame(false, true, true, true, generationUsage())); err != nil {
			t.Fatal(err)
		}
		timing.finish(true)
		if seconds <= 0 {
			if timing.snapshot().TPS() != nil {
				t.Fatal("invalid clock measured")
			}
		} else {
			assertGeneration(t, timing.snapshot(), 32.5)
		}
	}
}
