package aistudio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
)

func TestBuildUnaryFallbackStaysPinnedAndCountsTwoSends(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelBuild)
	fixture.reply = func(request GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
		if index == 1 {
			if rpc.Method != buildProxyUnaryMethod {
				t.Fatal(rpc.Method)
			}
			return nil, &RPCError{StatusCode: 501, Code: 12, Message: "unary unsupported; use stream"}
		}
		if index != 2 || rpc.Method != buildProxyStreamedMethod {
			t.Fatal("cross-channel or excess send", index, rpc.Method)
		}
		return fixtureResponse(t, rpc, []Part{fixtureTransportPart(t, request, "answer")}, "stop", &Usage{TotalTokens: 12}, ""), nil
	}
	events := fixture.generate(t, GenerateRequest{Model: "build/anti-truncation/fixture-model", Unary: true})
	if pipelineError(events) != nil || pipelineFinish(events) == nil || pipelineText(events) != "answer" {
		t.Fatal(events)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.sends) != 2 || len(fixture.attempts) != 2 {
		t.Fatal("physical mode fallback not counted")
	}
	var fallback bool
	for _, attempt := range fixture.attempts {
		if attempt.Channel != ChannelBuild || attempt.RequestedChannel != ChannelBuild || attempt.AccountID != fixture.account.ID {
			t.Fatalf("lost pin: %+v", attempt)
		}
		if attempt.Reason == "build_mode_fallback" && attempt.Mode == "stream" {
			fallback = true
		}
	}
	if !fallback {
		t.Fatal("missing mode fallback reason")
	}
}

type cancellationFixtureBody struct {
	first  *bytes.Reader
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (body *cancellationFixtureBody) Read(buffer []byte) (int, error) {
	if body.first.Len() > 0 {
		return body.first.Read(buffer)
	}
	select {
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	case <-body.closed:
		return 0, io.ErrClosedPipe
	}
}
func (body *cancellationFixtureBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

func TestCancellationAfterSemanticOutputDoesNotReplayOrFinish(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		t.Run(string(channel), func(t *testing.T) {
			fixture := newPipelineFixture(t, channel)
			ctx, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			fixture.client.protected = ProtectedTransportFunc(func(callCtx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
				callCtx, attempt := beginPhysicalAttempt(callCtx, request, rpc)
				markPhysicalSend(callCtx)
				fixture.mu.Lock()
				fixture.sends = append(fixture.sends, rpc)
				fixture.mu.Unlock()
				body := fixtureResponse(t, rpc, []Part{{Text: "first"}}, "", nil, "")
				return attempt.attach(&RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: &cancellationFixtureBody{first: bytes.NewReader(body), ctx: callCtx, closed: make(chan struct{})}}), nil
			})
			request := GenerateRequest{ID: t.Name(), AccountID: fixture.account.ID, Model: string(channel) + "/fixture-model", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture"}}}}}
			events, err := fixture.client.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			first := <-events
			if first.Kind != EventText || first.Text != "first" {
				t.Fatal(first)
			}
			cancel()
			for event := range events {
				if event.Kind == EventFinish {
					t.Fatal("cancelled stream forged a finish")
				}
				if event.Kind == EventError && !errors.Is(event.Err, context.Canceled) {
					t.Fatal(event.Err)
				}
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if len(fixture.sends) != 1 || len(fixture.attempts) != 1 {
				t.Fatal("cancel replayed")
			}
			for _, attempt := range fixture.attempts {
				if attempt.Stage != "cancelled" || attempt.Channel != channel || !attempt.Sent {
					t.Fatalf("%+v", attempt)
				}
			}
		})
	}
}
