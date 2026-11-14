package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// PR #100 fixtures use only the existing in-memory protected transport.
func malformedFixture(t *testing.T, rpc RPCRequest, parts []Part, finish string, usage *Usage, source string) []byte {
	t.Helper()
	fixtureParts := parts
	if len(fixtureParts) == 0 && rpc.Method == "GenerateContent" {
		fixtureParts = []Part{{Text: "fixture-placeholder"}}
	}
	var root []any
	if err := json.Unmarshal(fixtureResponse(t, rpc, fixtureParts, finish, usage, ""), &root); err != nil {
		t.Fatal(err)
	}
	if rpc.Method == "GenerateContent" {
		candidate := root[0].([]any)[0].([]any)[0].([]any)[0].([]any)
		if len(parts) == 0 {
			candidate[0] = nil
		}
		if finish == "malformed_response" {
			candidate[1] = 19
		}
		if source != "" {
			candidate[6] = []any{[]any{[]any{nil, nil, source, "fixture"}}}
			candidate[7] = []any{nil, []any{[]any{[]any{source, "fixture", "text"}}}}
		}
	} else {
		container := root
		if rpc.Method != buildProxyUnaryMethod {
			container = root[0].([]any)[0].([]any)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(container[0].(string)), &payload); err != nil {
			t.Fatal(err)
		}
		candidate := payload["candidates"].([]any)[0].(map[string]any)
		candidate["finishReason"] = strings.ToUpper(finish)
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		container[0] = string(encoded)
	}
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestMalformedResponseMappings(t *testing.T) {
	if decodeFinishReason(19) != "malformed_response" || buildFinishReason("MALFORMED_RESPONSE") != "malformed_response" || antiObservedFinishReason("malformed_response") != "malformed_response" {
		t.Fatal("19 mapping or audit label lost")
	}
	if decodeFinishReason(99) != "provider_99" || decodeFinishReason(3) != "safety" {
		t.Fatal("unrelated finish mapping changed")
	}
}

func TestMalformedResponseRegenerationMatrix(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, unary := range []bool{false, true} {
			for _, anti := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/unary=%v/anti=%v", channel, unary, anti), func(t *testing.T) {
					fixture := newPipelineFixture(t, channel)
					fixture.reply = func(request GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
						finish := "malformed_response"
						var parts []Part
						if index == 2 {
							finish, parts = "stop", []Part{{Text: "recovered"}}
							if anti {
								parts = []Part{fixtureTransportPart(t, request, "recovered")}
							}
						}
						return malformedFixture(t, rpc, parts, finish, &Usage{InputTokens: int64(index * 10), TotalTokens: int64(index * 10)}, ""), nil
					}
					model := string(channel) + "/"
					if anti {
						model += "anti-truncation/"
					}
					events := fixture.generate(t, GenerateRequest{Model: model + "fixture-model", Unary: unary})
					if pipelineError(events) != nil || pipelineText(events) != "recovered" || pipelineFinish(events).FinishReason != "stop" {
						t.Fatalf("recovery failed: %+v", events)
					}
					if len(fixture.sends) != 2 || len(fixture.attempts) != 2 || !bytes.Equal(fixture.sends[0].Body, fixture.sends[1].Body) {
						t.Fatal("replay changed request or physical attempt count")
					}
					if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 20 {
						t.Fatalf("stale public usage: %+v", usage)
					}
					retries, total := 0, int64(0)
					for _, attempt := range fixture.attempts {
						if attempt.AccountID != fixture.account.ID || attempt.Channel != channel || !attempt.Sent || attempt.Stage != "upstream_finished" || attempt.Usage == nil {
							t.Fatalf("physical ledger lost: %+v", attempt)
						}
						total += attempt.Usage.TotalTokens
						if attempt.Reason == "malformed_response_regeneration" {
							retries++
						}
					}
					if retries != 1 || total != 30 {
						t.Fatalf("attempt provenance retries=%d usage=%d", retries, total)
					}
				})
			}
		}
	}
}

func TestMalformedResponseRegenerationCapAndSendFailure(t *testing.T) {
	for _, mode := range []string{"cap", "send_failure", "missing_final_usage"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			sentinel := errors.New("fixture resend failed")
			fixture.reply = func(_ GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
				if index == 2 && mode == "send_failure" {
					return nil, sentinel
				}
				if index == 2 && mode == "missing_final_usage" {
					return malformedFixture(t, rpc, []Part{{Text: "done"}}, "stop", nil, ""), nil
				}
				return malformedFixture(t, rpc, nil, "malformed_response", &Usage{InputTokens: int64(index * 100), TotalTokens: int64(index * 100)}, ""), nil
			}
			events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model"})
			switch mode {
			case "cap":
				if len(fixture.sends) != 4 || pipelineError(events) != nil || pipelineFinish(events).FinishReason != "malformed_response" || pipelineUsage(events).TotalTokens != 400 {
					t.Fatalf("unbounded/lost exhausted finish: %+v", events)
				}
			case "send_failure":
				if len(fixture.sends) != 2 || !errors.Is(pipelineError(events), sentinel) || pipelineFinish(events) != nil || pipelineUsage(events) != nil {
					t.Fatalf("send failure masked/stale usage: %+v", events)
				}
			case "missing_final_usage":
				if len(fixture.sends) != 2 || pipelineError(events) != nil || pipelineUsage(events).Source != "estimated" || pipelineUsage(events).TotalTokens == 100 {
					t.Fatalf("raw observer retained prior usage: %+v", events)
				}
			}
		})
	}
}

func TestMalformedResponseCommittedOutputNeverReplayed(t *testing.T) {
	for _, mode := range []string{"text", "reasoning", "signature", "hidden_signed_reasoning", "tool", "code", "code_result", "media", "stop_prefix", "anti_text", "anti_empty_call"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			request := GenerateRequest{Model: "playground/fixture-model"}
			if strings.HasPrefix(mode, "anti_") {
				request.Model = "playground/anti-truncation/fixture-model"
			}
			if mode == "stop_prefix" {
				request.Config.StopSequences = []string{"STOP"}
			}
			if mode == "hidden_signed_reasoning" {
				request.Config.HideThinking = true
			}
			fixture.reply = func(upstream GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
				part := Part{Text: "shown"}
				switch mode {
				case "reasoning":
					part.Thought = true
				case "signature":
					part = Part{ThoughtSignature: "fixture-signature"}
				case "hidden_signed_reasoning":
					part.Thought, part.ThoughtSignature = true, "fixture-signature"
				case "tool":
					part = Part{FunctionCall: &FunctionCall{Name: "real", Arguments: json.RawMessage("{}")}}
				case "code":
					part = Part{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "print(1)"}}
				case "code_result":
					part = Part{CodeExecutionResult: &CodeExecutionResult{Outcome: "OUTCOME_OK", Output: "1"}}
				case "media":
					part = Part{InlineData: &Blob{MIME: "image/png", Data: []byte("fixture")}}
				case "stop_prefix":
					part.Text = "ST"
				case "anti_empty_call":
					part = fixtureTransportPart(t, upstream, "")
				}
				return malformedFixture(t, rpc, []Part{part}, "malformed_response", &Usage{InputTokens: 10, TotalTokens: 10}, ""), nil
			}
			events := fixture.generate(t, request)
			if len(fixture.sends) != 1 || pipelineError(events) != nil || pipelineFinish(events) == nil || pipelineFinish(events).FinishReason != "malformed_response" {
				t.Fatalf("committed %s replayed or corrupted: %+v", mode, events)
			}
			if mode == "stop_prefix" && pipelineText(events) != "ST" {
				t.Fatal("existing stop prefix flush changed")
			}
		})
	}
}

func TestMalformedResponseHiddenThinkingAndSources(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprint(exhausted), func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			fixture.reply = func(_ GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
				finish, parts := "malformed_response", []Part{{Text: "hidden", Thought: true}}
				if !exhausted && index == 2 {
					finish, parts = "stop", []Part{{Text: "visible"}}
				}
				return malformedFixture(t, rpc, parts, finish, &Usage{InputTokens: 1, TotalTokens: 1}, fmt.Sprintf("https://fixture.invalid/%d", index)), nil
			}
			events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model", Config: GenerationConfig{HideThinking: true}})
			wantSends := 2
			if exhausted {
				wantSends = 4
			}
			if len(fixture.sends) != wantSends || pipelineError(events) != nil {
				t.Fatalf("hidden-only recovery failed: %+v", events)
			}
			citations, groundings := 0, 0
			for _, event := range events {
				if event.Kind == EventReasoning {
					t.Fatal("hidden reasoning leaked")
				}
				if event.Citation != nil {
					citations++
					if !strings.HasSuffix(event.Citation.URL, fmt.Sprint(wantSends)) {
						t.Fatal("discarded citation leaked")
					}
				}
				if event.Grounding != nil {
					groundings++
				}
			}
			if citations != 1 || groundings != 1 {
				t.Fatalf("final sources lost: citations=%d grounding=%d", citations, groundings)
			}
		})
	}
}

func TestMalformedResponseBrokenEnvelopeAndCancellationNeverReplay(t *testing.T) {
	for _, mode := range []string{"broken_root", "cancelled", "safety"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			ctx, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			fixture.ctx = ctx
			fixture.reply = func(_ GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
				if mode == "safety" {
					return malformedFixture(t, rpc, nil, "safety", nil, ""), nil
				}
				body := malformedFixture(t, rpc, nil, "malformed_response", nil, "")
				if mode == "broken_root" {
					body = body[:len(body)-1]
				} else {
					cancel()
				}
				return body, nil
			}
			events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model"})
			if len(fixture.sends) != 1 {
				t.Fatal("error/cancellation/safety was replayed")
			}
			if mode == "broken_root" && (pipelineError(events) == nil || pipelineFinish(events) != nil) {
				t.Fatalf("19 masked malformed envelope: %+v", events)
			}
		})
	}
}

func TestMalformedResponseTrailerStatus(t *testing.T) {
	for _, finish := range []string{"malformed_response", "stop"} {
		for _, trailer := range []string{"absent", "null", "zero", "503", "invalid", "other_field"} {
			t.Run(finish+"/"+trailer, func(t *testing.T) {
				fixture := newPipelineFixture(t, ChannelPlayground)
				fixture.reply = func(_ GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
					reason := finish
					var parts []Part
					if finish == "stop" || index > 1 {
						reason, parts = "stop", []Part{{Text: "visible"}}
					}
					var root []any
					if err := json.Unmarshal(malformedFixture(t, rpc, parts, reason, nil, ""), &root); err != nil {
						t.Fatal(err)
					}
					switch trailer {
					case "null":
						root = append(root, nil)
					case "zero":
						root = append(root, []any{0})
					case "503":
						root = append(root, []any{14, "fixture unavailable"})
					case "invalid":
						root = append(root, "invalid")
					case "other_field":
						root = append(root, nil, []any{14, "not a status field"})
					}
					return json.Marshal(root)
				}
				events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model"})
				failed := trailer == "503" || trailer == "invalid"
				wantSends := 1
				if finish == "malformed_response" && !failed {
					wantSends = 2
				}
				if len(fixture.sends) != wantSends {
					t.Fatalf("trailer replay count=%d want=%d", len(fixture.sends), wantSends)
				}
				err := pipelineError(events)
				if failed != (err != nil) || failed && pipelineFinish(events) != nil {
					t.Fatalf("trailer error/finish wrong: %+v", events)
				}
				if trailer == "503" {
					var rpcError *RPCError
					if !errors.As(err, &rpcError) || rpcError.StatusCode != 503 || rpcError.Method != "GenerateContent" {
						t.Fatalf("wrong RPC status: %v", err)
					}
				}
				if trailer == "invalid" {
					var evidence *ProtocolEvidenceError
					if !errors.As(err, &evidence) || evidence.Method != "GenerateContent" {
						t.Fatalf("wrong evidence method: %v", err)
					}
				}
				if finish == "stop" && pipelineText(events) != "visible" {
					t.Fatal("trailer discarded already emitted text")
				}
			})
		}
	}
}

func TestMalformedResponseCancelledAfterTerminalNeverReplays(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelPlayground)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	fixture.ctx = ContextWithGenerationAttemptObserver(ctx, func(attempt GenerationAttempt) {
		if attempt.Stage == "upstream_finished" {
			cancel()
		}
	})
	fixture.reply = func(_ GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
		return malformedFixture(t, rpc, nil, "malformed_response", nil, ""), nil
	}
	_ = fixture.generate(t, GenerateRequest{Model: "playground/fixture-model"})
	if len(fixture.sends) != 1 {
		t.Fatal("cancelled successful decode replayed")
	}
}

type malformedCancelBody struct {
	io.ReadCloser
	closed  chan struct{}
	reading chan struct{}
	once    sync.Once
}

func (body *malformedCancelBody) Read(buffer []byte) (int, error) {
	if body.reading != nil {
		close(body.reading)
		<-body.closed
		return 0, context.Canceled
	}
	return body.ReadCloser.Read(buffer)
}

func (body *malformedCancelBody) Close() error {
	body.once.Do(func() {
		close(body.closed)
		_ = body.ReadCloser.Close()
	})
	return nil
}

func TestMalformedResponseCancellationClosesCurrentRetryBody(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelPlayground)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	fixture.ctx = ctx
	first := &malformedCancelBody{closed: make(chan struct{})}
	second := &malformedCancelBody{closed: make(chan struct{}), reading: make(chan struct{})}
	original := fixture.client.protected
	index := 0
	fixture.client.protected = ProtectedTransportFunc(func(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
		response, err := original.DoProtected(ctx, request, rpc)
		if err != nil {
			return nil, err
		}
		index++
		body := first
		if index == 2 {
			body = second
		}
		body.ReadCloser, response.Body = response.Body, body
		return response, nil
	})
	fixture.reply = func(_ GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
		return malformedFixture(t, rpc, nil, "malformed_response", nil, ""), nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fixture.generate(t, GenerateRequest{Model: "playground/fixture-model"})
	}()
	select {
	case <-second.reading:
	case <-time.After(2 * time.Second):
		t.Fatal("second generation did not start")
	}
	select {
	case <-first.closed:
	default:
		t.Error("first body was not closed before retry")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = second.Close()
		<-done
		t.Fatal("cancellation did not close current retry body")
	}
	select {
	case <-second.closed:
	default:
		t.Fatal("current body remained open")
	}
	if len(fixture.sends) != 2 {
		t.Fatal("cancelled retry sent again")
	}
}
