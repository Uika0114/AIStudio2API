package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Deliver a complete synthetic RPC envelope, but withhold transport EOF until
// Close or cancellation. A protocol-complete response must not read this tail.
// No network, credentials, WAA worker or real Applet is used.
type assistantCompletionBody struct {
	data   *bytes.Reader
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
	chunk  int
}

func newAssistantCompletionBody(ctx context.Context, data []byte, chunk int) *assistantCompletionBody {
	return &assistantCompletionBody{data: bytes.NewReader(data), ctx: ctx, closed: make(chan struct{}), chunk: chunk}
}

func (body *assistantCompletionBody) Read(buffer []byte) (int, error) {
	if body.chunk > 0 && len(buffer) > body.chunk {
		buffer = buffer[:body.chunk]
	}
	if n, err := body.data.Read(buffer); n > 0 || err != io.EOF {
		return n, err
	}
	select {
	case <-body.closed:
		return 0, io.EOF
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	}
}

func (body *assistantCompletionBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

type assistantCompletionResult struct {
	events []Event
	err    error
}

func TestAssistantCompletionRootWithoutTransportEOF(t *testing.T) {
	for _, chunk := range []int{0, 1, 7} {
		for _, trailer := range []bool{false, true} {
			t.Run(fmt.Sprintf("chunk=%d/trailer=%v", chunk, trailer), func(t *testing.T) {
				root := []any{[]any{assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("answer", false)))}}
				if trailer {
					root = append(root, []any{0})
				}
				wire, err := json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				body := newAssistantCompletionBody(context.Background(), wire, chunk)
				defer body.Close()
				done := make(chan assistantCompletionResult, 1)
				go func() {
					var result assistantCompletionResult
					result.err = DecodeAssistantStream(body, nil, func(event Event) error {
						result.events = append(result.events, event)
						return nil
					})
					done <- result
				}()
				var result assistantCompletionResult
				select {
				case result = <-done:
				case <-time.After(time.Second):
					_ = body.Close()
					<-done // Join the decoder before reporting the regression.
					t.Fatal("complete Assistant root array still waits for transport EOF")
				}
				if result.err != nil || assistantText(result.events) != "answer" || assistantFinish(result.events) == nil {
					t.Fatalf("complete response did not finish: text=%q err=%v", assistantText(result.events), result.err)
				}
				usage := assistantUsage(result.events)
				if usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
					t.Fatalf("usage lost: %+v", usage)
				}
				finishes := 0
				for _, event := range result.events {
					if event.Kind == EventFinish {
						finishes++
					}
				}
				if finishes != 1 {
					t.Fatalf("finish count=%d", finishes)
				}
			})
		}
	}
}

func TestAssistantCompletionStillRejectsInvalidTermination(t *testing.T) {
	finished := assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("answer", false))
	complete := assistantStreamBytes(assistantMessage(finished))
	trailer, err := json.Marshal([]any{[]any{assistantMessage(finished)}, []any{8, "fixture quota error"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, detail string
		body         []byte
	}{
		{"missing-root", "根数组没有正常结束", complete[:len(complete)-1]},
		{"broken-message", "unexpected EOF", []byte(`[[[null,null,[`)},
		{"error-trailer", "fixture quota error", trailer},
		{"no-completed-model", "流结束前没有完成的模型回复", assistantStreamBytes(assistantMessage(assistantStep("model", 1, 2, nil, assistantPart("partial", false))))},
		{"later-pending-model", "流结束前仍有未完成步骤", assistantStreamBytes(assistantMessage(finished), assistantMessage(assistantStep("model", 2, 2, nil, assistantPart("more", false))))},
		{"later-pending-control", "流结束前仍有未完成步骤", assistantStreamBytes(assistantMessage(finished), assistantMessage(assistantStep("user", 2, 2, nil)))},
		{"unknown-status", "未知的步骤状态", assistantStreamBytes(assistantMessage(assistantStep("model", 1, 3, nil, assistantPart("answer", false))))},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			events, err := assistantCollect(t, bytes.NewReader(test.body))
			if err == nil || !strings.Contains(err.Error(), test.detail) || assistantFinish(events) != nil {
				t.Fatalf("invalid termination became success: err=%v finish=%+v", err, assistantFinish(events))
			}
		})
	}
}

func TestAssistantCompletionErrorTrailerDoesNotWaitForEOF(t *testing.T) {
	wire, err := json.Marshal([]any{[]any{assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("answer", false)))}, []any{8, "fixture quota error"}})
	if err != nil {
		t.Fatal(err)
	}
	body := newAssistantCompletionBody(context.Background(), wire, 1)
	defer body.Close()
	done := make(chan assistantCompletionResult, 1)
	go func() {
		var result assistantCompletionResult
		result.err = DecodeAssistantStream(body, nil, func(event Event) error {
			result.events = append(result.events, event)
			return nil
		})
		done <- result
	}()
	select {
	case result := <-done:
		var rpcError *RPCError
		if !errors.As(result.err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests || assistantFinish(result.events) != nil {
			t.Fatalf("trailer error lost: %v", result.err)
		}
		if usage := assistantUsage(result.events); usage == nil || usage.TotalTokens != 17 {
			t.Fatalf("known usage lost on error: %+v", usage)
		}
	case <-time.After(time.Second):
		_ = body.Close()
		<-done
		t.Fatal("error trailer waits for EOF")
	}
}

// A completed model step is not by itself the end of the stream. Without a
// closed root envelope we must still wait, and cancellation/EOF must not turn
// the partial transport into a successful response.
func TestAssistantCompletionOpenRootNeverSynthesizesFinish(t *testing.T) {
	wire := assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("answer", false))))
	body := newAssistantCompletionBody(context.Background(), wire[:len(wire)-1], 1)
	defer body.Close()
	done := make(chan assistantCompletionResult, 1)
	text := make(chan struct{}, 1)
	go func() {
		var result assistantCompletionResult
		result.err = DecodeAssistantStream(body, nil, func(event Event) error {
			result.events = append(result.events, event)
			if event.Kind == EventText {
				text <- struct{}{}
			}
			return nil
		})
		done <- result
	}()
	select {
	case <-text:
	case <-time.After(time.Second):
		_ = body.Close()
		<-done
		t.Fatal("partial envelope did not emit its text")
	}
	select {
	case result := <-done:
		t.Fatalf("open root returned before cancellation/EOF: err=%v", result.err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = body.Close()
	result := <-done
	if result.err == nil || assistantFinish(result.events) != nil {
		t.Fatalf("open root became success: err=%v", result.err)
	}
	if usage := assistantUsage(result.events); usage == nil || usage.TotalTokens != 17 {
		t.Fatalf("known usage lost: %+v", usage)
	}
}

type assistantCompletionTransport struct {
	fixture *assistantFixture
	body    []byte
	chunk   int
}

func (transport assistantCompletionTransport) Do(ctx context.Context, request RPCRequest) (*RPCResponse, error) {
	if request.Method == assistantStreamMethod {
		return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: newAssistantCompletionBody(ctx, transport.body, transport.chunk)}, nil
	}
	return (assistantAuxTransport{fixture: transport.fixture}).Do(ctx, request)
}

func TestAssistantCompletionClientClosesWithoutTransportEOF(t *testing.T) {
	for _, unary := range []bool{false, true} {
		t.Run(fmt.Sprint(unary), func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			fixture.client.transport = assistantCompletionTransport{fixture: fixture, chunk: 7, body: assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("answer", false))))}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			request := assistantCoreRequest(lease)
			request.Unary = unary
			source, err := fixture.client.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan []Event, 1)
			go func() {
				var events []Event
				for event := range source {
					events = append(events, event)
				}
				done <- events
			}()
			var events []Event
			select {
			case events = <-done:
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("Client.Generate still waits for EOF instead of closing its event source")
			}
			if pipelineError(events) != nil || pipelineText(events) != "answer" || pipelineFinish(events) == nil {
				t.Fatalf("terminal projection: %+v", events)
			}
			if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
				t.Fatalf("usage: %+v", usage)
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			_, protected, attempts := fixture.snapshot()
			if len(protected) != 1 || len(attempts) != 1 {
				t.Fatal("completion caused another generation")
			}
			fixture.mu.Lock()
			calls := append([]string(nil), fixture.cleanupCalls...)
			cleanupLease := fixture.cleanupLease
			fixture.mu.Unlock()
			if len(calls) != 1 || calls[0] != assistantDeleteAppletMethod || cleanupLease != lease {
				t.Fatalf("cleanup changed: calls=%v same-lease=%v", calls, cleanupLease == lease)
			}
		})
	}
}
