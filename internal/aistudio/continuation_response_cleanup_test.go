package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type continuationResponseBody struct {
	io.ReadCloser
	closes      atomic.Int32
	readStarted chan struct{}
	readOnce    sync.Once
}

func (body *continuationResponseBody) Read(buffer []byte) (int, error) {
	if body.readStarted != nil {
		body.readOnce.Do(func() { close(body.readStarted) })
	}
	return body.ReadCloser.Read(buffer)
}

func (body *continuationResponseBody) Close() error {
	body.closes.Add(1)
	return body.ReadCloser.Close()
}

func TestPlaygroundContinuationResponseCleanup(t *testing.T) {
	for _, mode := range []string{"success", "send_failure", "decode_failure"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			failure := errors.New("fixture next-round send failure")
			fixture.reply = func(_ GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
				if mode == "send_failure" && index == 3 {
					return nil, failure
				}
				if mode == "decode_failure" && index == 2 {
					return []byte("{"), nil
				}
				finish, token := "continue", fmt.Sprintf("fixture-token-%d", index)
				if index == 4 {
					finish, token = "stop", ""
				}
				return fixtureResponse(t, rpc, []Part{{Text: fmt.Sprint(index)}}, finish,
					&Usage{InputTokens: int64(10 * index), OutputTokens: int64(index), TotalTokens: int64(11 * index)}, token), nil
			}
			original := fixture.client.protected
			var bodies []*continuationResponseBody
			fixture.client.protected = ProtectedTransportFunc(func(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
				// The initial response still belongs to Generate. Only responses
				// opened by decodePlaygroundRounds must close before the next send.
				if len(bodies) > 1 && bodies[len(bodies)-1].closes.Load() == 0 {
					t.Errorf("continuation round %d remained open at the next send", len(bodies)-1)
				}
				response, err := original.DoProtected(ctx, request, rpc)
				if err != nil {
					return nil, err
				}
				body := &continuationResponseBody{ReadCloser: response.Body}
				bodies = append(bodies, body)
				response.Body = body
				return response, nil
			})
			budget := int64(65536)
			events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}})
			for index, body := range bodies {
				if body.closes.Load() == 0 {
					t.Errorf("response %d remained open after generation returned", index)
				}
			}
			switch mode {
			case "success":
				if len(fixture.sends) != 4 || pipelineError(events) != nil || pipelineText(events) != "1234" || pipelineFinish(events) == nil {
					t.Fatalf("continuation output changed: %+v", events)
				}
				if usage := pipelineUsage(events); usage == nil || usage.OutputTokens != 10 || usage.TotalTokens != 50 {
					t.Fatalf("continuation aggregate changed: %+v", usage)
				}
				if len(fixture.attempts) != 4 {
					t.Fatalf("physical attempt count = %d", len(fixture.attempts))
				}
				for _, attempt := range fixture.attempts {
					if attempt.Usage == nil || attempt.Usage.TotalTokens != int64(11*(attempt.ContinuationRound+1)) {
						t.Fatalf("physical usage changed: %+v", attempt)
					}
				}
			case "send_failure":
				if len(fixture.sends) != 3 || !errors.Is(pipelineError(events), failure) || pipelineFinish(events) != nil {
					t.Fatalf("next-round send failure lost: %+v", events)
				}
			case "decode_failure":
				if len(fixture.sends) != 2 || pipelineError(events) == nil || pipelineFinish(events) != nil {
					t.Fatalf("continuation decode failure lost: %+v", events)
				}
			}
		})
	}
}

func TestPlaygroundContinuationCancellationClosesBlockedResponse(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelPlayground)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	fixture.ctx = ctx
	fixture.reply = func(_ GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
		return fixtureResponse(t, rpc, []Part{{Text: "first"}}, "continue", nil, "fixture-token"), nil
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	blocked := &continuationResponseBody{ReadCloser: reader, readStarted: make(chan struct{})}
	original := fixture.client.protected
	fixture.client.protected = ProtectedTransportFunc(func(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
		response, err := original.DoProtected(ctx, request, rpc)
		if err == nil && request.continuationRound == 1 {
			_ = response.Body.Close()
			response.Body = blocked
		}
		return response, err
	})
	budget := int64(65536)
	events, err := fixture.client.Generate(ctx, GenerateRequest{
		ID: t.Name(), AccountID: fixture.account.ID, Model: "playground/fixture-model",
		Config:   GenerationConfig{MaxOutputTokens: &budget},
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for range events {
		}
		close(done)
	}()
	select {
	case <-blocked.readStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation did not reach its blocked read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not release continuation")
	}
	if blocked.closes.Load() == 0 || len(fixture.sends) != 2 {
		t.Fatalf("cancel cleanup failed: closes=%d sends=%d", blocked.closes.Load(), len(fixture.sends))
	}
}
