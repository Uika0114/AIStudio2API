package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestAntiObservationPipeline(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, kind := range []string{"legal", "empty", "fallback", "real_tool", "invalid", "same_frame_duplicate"} {
			t.Run(string(channel)+"/"+kind, func(t *testing.T) {
				f := newPipelineFixture(t, channel)
				var mu sync.Mutex
				var last AntiTruncationObservation
				f.ctx = ContextWithAntiTruncationObserver(f.ctx, func(v AntiTruncationObservation) {
					mu.Lock()
					defer mu.Unlock()
					if v.Revision > last.Revision {
						last = v.Clone()
					}
				})
				f.reply = func(r GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
					part := fixtureTransportPart(t, r, "body-private-sentinel")
					parts := []Part{part}
					switch kind {
					case "empty":
						parts = []Part{fixtureTransportPart(t, r, "")}
					case "fallback":
						parts = []Part{{Text: "ordinary-private-sentinel"}}
					case "real_tool":
						parts = []Part{{FunctionCall: &FunctionCall{Name: "real_tool", Arguments: json.RawMessage(`{}`)}}}
					case "invalid":
						part.FunctionCall.Arguments = json.RawMessage(`{"other":true}`)
						parts = []Part{part}
					case "same_frame_duplicate":
						parts = []Part{part, part, part}
					}
					return fixtureResponse(t, rpc, parts, "safety", nil, ""), nil
				}
				events := f.generate(t, GenerateRequest{Model: string(channel) + "/anti-truncation/fixture-model", Tools: Tools{Functions: []FunctionDeclaration{{Name: "real_tool", Parameters: json.RawMessage(`{"type":"object"}`)}}}})
				mu.Lock()
				v := last.Clone()
				mu.Unlock()
				if v.Version != 1 || !v.Injected || !v.Sent || v.End == "preparing" || len(v.Rounds) != 1 {
					t.Fatalf("missing observation: %+v", v)
				}
				r := v.Rounds[0]
				if r.FinishReason != "safety" {
					t.Fatalf("raw terminal lost: %+v", v)
				}
				switch kind {
				case "legal", "empty":
					if r.Calls != 1 || r.Legal != 1 || !v.Complete || r.Fallback {
						t.Fatalf("legal: %+v", v)
					}
					if (kind == "empty") != (r.Empty == 1) {
						t.Fatal("empty conflated")
					}
				case "fallback":
					if r.Calls != 0 || !r.Fallback || !r.OrdinaryText || !v.Complete {
						t.Fatalf("fallback: %+v", v)
					}
				case "real_tool":
					if r.Calls != 0 || r.RealTools != 1 || r.Fallback || !v.Complete {
						t.Fatalf("tool-only: %+v", v)
					}
				case "invalid":
					if r.Calls != 1 || r.Invalid != 1 || v.Complete || pipelineError(events) == nil {
						t.Fatalf("invalid: %+v", v)
					}
				case "same_frame_duplicate":
					if r.Calls != 3 || r.Legal != 1 || r.Rejected != 1 || r.Invalid != 0 || v.Complete || pipelineError(events) == nil {
						t.Fatalf("raw unprocessed call lost: %+v", v)
					}
				}
				encoded, _ := json.Marshal(v)
				for _, secret := range []string{"private-sentinel", "aistudio2api_emit_v1", "internal-only-signature", "\"arguments\""} {
					if strings.Contains(string(encoded), secret) {
						t.Fatalf("observation leak: %s", encoded)
					}
				}
			})
		}
	}
}

func TestAntiObservationLifetimeAndIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first, second AntiTruncationObservation
	r, _ := NormalizeGenerateRequestModel(GenerateRequest{ID: "same-public-id", Model: "playground/anti-truncation/fixture-model"})
	c1, o1 := beginAntiObservation(ContextWithAntiTruncationObserver(ContextWithGenerationPurpose(ctx, "compaction"), func(v AntiTruncationObservation) { first = v }), r, false)
	_, o2 := beginAntiObservation(ContextWithAntiTruncationObserver(ctx, func(v AntiTruncationObservation) { second = v }), r, true)
	o1.injected("private-name")
	o1.attempt(GenerationAttempt{ID: "one", Sent: true})
	o1.raw("one", Event{Kind: EventToolCall, ToolCall: &FunctionCall{Name: "private-name"}})
	o1.raw("one", Event{Kind: EventFinish, FinishReason: "provider_19"})
	o1.decoded("one", errors.New("bad trailer"), false)
	o1.finish(errors.New("bad trailer"))
	o2.finish(context.Canceled)
	if first.ID == second.ID || first.Purpose != "compaction" || second.Purpose != "reply" || first.SystemPrompt || !second.SystemPrompt {
		t.Fatal("identity/policy conflated")
	}
	if first.Complete || first.Rounds[0].Calls != 1 || first.Rounds[0].FinishReason != "provider_19" || second.Sent || second.End != "not_sent" {
		t.Fatalf("incomplete fabricated: %+v %+v", first, second)
	}
	first.Rounds[0].Calls = 99
	var snap AntiTruncationObservation
	o1.callback = func(v AntiTruncationObservation) { snap = v }
	o1.change(func(*AntiTruncationObservation) {})
	if snap.Rounds[0].Calls != 1 {
		t.Fatal("snapshot aliases mutable round")
	}
	_ = c1
}

func TestAntiObservationContinuationPerSend(t *testing.T) {
	f := newPipelineFixture(t, ChannelPlayground)
	var last AntiTruncationObservation
	var mu sync.Mutex
	f.ctx = ContextWithAntiTruncationObserver(f.ctx, func(v AntiTruncationObservation) {
		mu.Lock()
		defer mu.Unlock()
		if v.Revision > last.Revision {
			last = v
		}
	})
	budget := int64(65536)
	f.reply = func(r GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
		if index == 1 {
			return fixtureResponse(t, rpc, []Part{{Text: "discarded draft"}}, "continue", nil, "next"), nil
		}
		return fixtureResponse(t, rpc, []Part{fixtureTransportPart(t, r, "reply")}, "stop", nil, ""), nil
	}
	events := f.generate(t, GenerateRequest{Model: "playground/anti-truncation/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}})
	mu.Lock()
	v := last.Clone()
	mu.Unlock()
	if pipelineError(events) != nil || pipelineText(events) != "reply" || len(v.Rounds) != 2 || !v.Complete || v.Rounds[0].Calls != 0 || v.Rounds[1].Calls != 1 || v.Rounds[0].Round != 0 || v.Rounds[1].Round != 1 {
		t.Fatalf("continuation: %+v", v)
	}
}

func TestAntiObservationTerminationAndCancellationBoundaries(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, kind := range []string{"bad_trailer", "missing_finish", "cancel_hit", "cancel_no_hit", "local_stop", "no_output", "collision", "unified_extra"} {
			t.Run(string(channel)+"/"+kind, func(t *testing.T) {
				f := newPipelineFixture(t, channel)
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				var mu sync.Mutex
				var last AntiTruncationObservation
				f.ctx = ContextWithAntiTruncationObserver(ctx, func(v AntiTruncationObservation) {
					mu.Lock()
					if v.Revision > last.Revision {
						last = v.Clone()
					}
					mu.Unlock()
					if len(v.Rounds) > 0 && ((kind == "cancel_hit" && v.Rounds[0].Calls > 0) || (kind == "cancel_no_hit" && v.Rounds[0].OrdinaryText)) {
						cancel()
					}
				})
				f.reply = func(r GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
					parts := []Part{fixtureTransportPart(t, r, "fixture STOP tail")}
					finish := "safety"
					if kind == "cancel_no_hit" {
						parts = []Part{{Text: "ordinary"}}
					}
					if kind == "no_output" {
						parts = []Part{{ThoughtSignature: "fixture-signature-only"}}
					}
					if kind == "missing_finish" {
						finish = ""
					}
					if kind == "collision" {
						parts = append([]Part{{FunctionCall: &FunctionCall{Name: antiTruncationTransportBaseName, Arguments: json.RawMessage(`{}`)}}}, parts...)
					}
					if kind == "unified_extra" {
						parts[0].FunctionCall.Arguments = json.RawMessage(`{"content":"fixture STOP tail","extra":"private"}`)
					}
					body := fixtureResponse(t, rpc, parts, finish, nil, "")
					if kind == "bad_trailer" {
						body = append(body, []byte(` invalid-trailer`)...)
					}
					return body, nil
				}
				r := GenerateRequest{Model: string(channel) + "/anti-truncation/fixture-model"}
				if kind == "local_stop" {
					r.Config.StopSequences = []string{"STOP"}
				}
				if kind == "collision" {
					r.Tools = Tools{Functions: []FunctionDeclaration{{Name: antiTruncationTransportBaseName, Parameters: json.RawMessage(`{"type":"object"}`)}}}
				}
				events := f.generate(t, r)
				mu.Lock()
				v := last.Clone()
				mu.Unlock()
				if len(v.Rounds) != 1 {
					t.Fatalf("missing physical evidence: %+v", v)
				}
				round := v.Rounds[0]
				switch kind {
				case "bad_trailer":
					if v.Complete || round.Calls != 1 || round.FinishReason != "safety" || pipelineError(events) == nil {
						t.Fatalf("bad trailer: %+v", v)
					}
				case "missing_finish":
					if v.Complete || round.Calls != 1 || pipelineError(events) == nil {
						t.Fatalf("missing finish: %+v", v)
					}
				case "cancel_hit":
					if v.Complete || round.Calls != 1 || v.End != "cancelled" {
						t.Fatalf("positive cancelled: %+v", v)
					}
				case "cancel_no_hit":
					if v.Complete || round.Calls != 0 || v.End != "cancelled" {
						t.Fatalf("unknown cancelled: %+v", v)
					}
				case "local_stop":
					if v.Complete || round.Calls != 1 || v.End != "local_stop" || round.FinishReason != "safety" {
						t.Fatalf("local stop: %+v", v)
					}
				case "no_output":
					if !v.Complete || round.Calls != 0 || round.RealTools != 0 || round.OrdinaryText {
						t.Fatalf("no output: %+v", v)
					}
				case "collision":
					if !v.Complete || round.Calls != 1 || round.RealTools != 1 {
						t.Fatalf("collision: %+v", v)
					}
				case "unified_extra":
					if v.Complete || round.Calls != 1 || round.Legal != 0 || round.Invalid != 1 || v.Behavior != "unified-reply-v2" || pipelineError(events) == nil {
						t.Fatalf("unified extra accepted: %+v", v)
					}
				}
			})
		}
	}
}

func TestAntiObservationContinuationNeverRecountsSharedHit(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_new_call", true: "second_independent_call"}[repeat], func(t *testing.T) {
			f := newPipelineFixture(t, ChannelPlayground)
			var last AntiTruncationObservation
			var mu sync.Mutex
			f.ctx = ContextWithAntiTruncationObserver(f.ctx, func(v AntiTruncationObservation) {
				mu.Lock()
				defer mu.Unlock()
				if v.Revision > last.Revision {
					last = v
				}
			})
			budget := int64(65536)
			f.reply = func(r GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
				if index == 1 {
					return fixtureResponse(t, rpc, []Part{fixtureTransportPart(t, r, "first")}, "continue", nil, "next"), nil
				}
				parts := []Part{{ThoughtSignature: "fixture-signature-only"}}
				if repeat {
					parts = []Part{fixtureTransportPart(t, r, "second")}
				}
				return fixtureResponse(t, rpc, parts, "stop", nil, ""), nil
			}
			events := f.generate(t, GenerateRequest{Model: "playground/anti-truncation/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}})
			mu.Lock()
			v := last.Clone()
			mu.Unlock()
			want := 0
			if repeat {
				want = 1
			}
			if len(v.Rounds) != 2 || v.Rounds[0].Calls != 1 || v.Rounds[1].Calls != want || v.Rounds[0].Legal != 1 {
				t.Fatalf("per-round count: %+v", v)
			}
			if repeat && (v.Complete || v.Rounds[1].Rejected != 1 || pipelineError(events) == nil) {
				t.Fatalf("repeat behavior changed: %+v", v)
			}
			if !repeat && (!v.Complete || pipelineError(events) != nil) {
				t.Fatalf("shared hit recounted: %+v", v)
			}
		})
	}
}

func TestAntiObservationNoPerTokenSnapshotsOrUntrustedFinishText(t *testing.T) {
	var last AntiTruncationObservation
	count := 0
	r, _ := NormalizeGenerateRequestModel(GenerateRequest{Model: "playground/anti-truncation/fixture-model"})
	_, o := beginAntiObservation(ContextWithAntiTruncationObserver(t.Context(), func(v AntiTruncationObservation) { last = v; count++ }), r, true)
	o.injected("private-name")
	o.attempt(GenerationAttempt{ID: "a", Sent: true})
	for i := 0; i < 1000; i++ {
		o.raw("a", Event{Kind: EventText, Text: "private-token"})
	}
	if count != 4 {
		t.Fatalf("per-token updates: %d", count)
	}
	o.raw("a", Event{Kind: EventFinish, FinishReason: "unsafe private-sentinel"})
	if last.Rounds[0].FinishReason != "provider_unknown" {
		t.Fatal("untrusted finish persisted")
	}
}

func TestAntiObservationAfterTerminalIsRejectedNotUnevaluated(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		t.Run(string(channel), func(t *testing.T) {
			f := newPipelineFixture(t, channel)
			var mu sync.Mutex
			var last AntiTruncationObservation
			f.ctx = ContextWithAntiTruncationObserver(f.ctx, func(v AntiTruncationObservation) {
				mu.Lock()
				defer mu.Unlock()
				if v.Revision > last.Revision {
					last = v
				}
			})
			f.reply = func(r GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
				first := fixtureResponse(t, rpc, []Part{{ThoughtSignature: "fixture-signature-only"}}, "stop", nil, "")
				second := fixtureResponse(t, rpc, []Part{fixtureTransportPart(t, r, "late-private")}, "", nil, "")
				var one, two []any
				json.Unmarshal(first, &one)
				json.Unmarshal(second, &two)
				one[0] = append(one[0].([]any), two[0].([]any)...)
				return json.Marshal(one)
			}
			events := f.generate(t, GenerateRequest{Model: string(channel) + "/anti-truncation/fixture-model"})
			mu.Lock()
			v := last.Clone()
			mu.Unlock()
			if len(v.Rounds) != 1 {
				t.Fatal("missing round")
			}
			r := v.Rounds[0]
			if v.Complete || r.Calls != 1 || r.Rejected != 1 || r.Invalid != 0 || r.RejectReason != "after_terminal" || pipelineError(events) == nil {
				t.Fatalf("after terminal classified unevaluated: %+v", v)
			}
		})
	}
}

func TestAntiObservationOtherOutputWithoutPayload(t *testing.T) {
	for _, kind := range []EventKind{EventExecutableCode, EventCodeExecutionResult, EventMedia, EventCitation, EventGrounding, EventThoughtSignature, EventToolCall, EventText} {
		t.Run(string(kind), func(t *testing.T) {
			var last AntiTruncationObservation
			r, _ := NormalizeGenerateRequestModel(GenerateRequest{Model: "playground/anti-truncation/fixture-model"})
			_, o := beginAntiObservation(ContextWithAntiTruncationObserver(t.Context(), func(v AntiTruncationObservation) { last = v }), r, true)
			o.injected("private")
			o.attempt(GenerationAttempt{ID: "a", Sent: true})
			o.raw("a", Event{Kind: kind, Text: "private-payload", ToolCall: &FunctionCall{Name: "real", Arguments: json.RawMessage(`{"sensitive":"sentinel"}`)}})
			o.raw("a", Event{Kind: EventFinish, FinishReason: "stop"})
			o.decoded("a", nil, false)
			o.finish(nil)
			want := kind == EventExecutableCode || kind == EventCodeExecutionResult || kind == EventMedia || kind == EventCitation || kind == EventGrounding
			if last.Rounds[0].OtherOutput != want || !last.Complete {
				t.Fatalf("other output: %+v", last)
			}
			b, _ := json.Marshal(last)
			if strings.Contains(string(b), "private-payload") || strings.Contains(string(b), "sentinel") {
				t.Fatal("payload persisted")
			}
		})
	}
}

func TestAntiObservationFinishReasonAllowlist(t *testing.T) {
	for _, v := range []string{"provider_synthetic_private_marker", "provider_", "provider_19_private", "provider_-1", "unsafe private", strings.Repeat("a", 81)} {
		if antiObservedFinishReason(v) != "provider_unknown" {
			t.Fatal("uncontrolled finish label", v)
		}
	}
	for _, v := range []string{"provider_19", "provider_22", "stop", "safety", "prohibited_content", "max_tokens"} {
		if antiObservedFinishReason(v) != v {
			t.Fatal("lost known finish", v)
		}
	}
}
