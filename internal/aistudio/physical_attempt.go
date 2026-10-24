package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type generationAttemptReasonKey struct{}
type physicalAttemptKey struct{}

// ContextWithGenerationAttemptReason labels an internal replay without exposing
// credentials, the prompt, tool arguments, upstream bodies or arbitrary headers.
func ContextWithGenerationAttemptReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, generationAttemptReasonKey{}, reason)
}

type physicalAttempt struct {
	mu       sync.Mutex
	ctx      context.Context
	value    GenerationAttempt
	channel  Channel
	terminal bool
	anti     *antiObservation
}

func generationRPCChannel(method string) Channel {
	if method == buildProxyStreamedMethod || method == buildProxyUnaryMethod {
		return ChannelBuild
	}
	if method == assistantInitMethod || method == assistantGenerateMethod {
		return ChannelAssistant
	}
	return ChannelPlayground
}

func beginPhysicalAttempt(ctx context.Context, request GenerateRequest, rpc RPCRequest) (context.Context, *physicalAttempt) {
	observer, _ := ctx.Value(generationAttemptObserverKey{}).(func(GenerationAttempt))
	anti, _ := ctx.Value(antiObservationKey{}).(*antiObservation)
	if observer == nil && anti == nil {
		return ctx, nil
	}
	now := time.Now()
	reason, _ := ctx.Value(generationAttemptReasonKey{}).(string)
	if reason == "" {
		if request.continuationRound > 0 {
			reason = "continuation"
		} else {
			reason = "initial"
		}
	}
	mode := "unary"
	if rpc.Streaming {
		mode = "stream"
	}
	requested, base := request.Selection.RequestedModel, request.Selection.BaseModel
	if requested == "" {
		requested = request.Model
	}
	if base == "" {
		base = request.Model
	}
	accountID := request.AccountID
	if lease, ok := AccountLeaseFromContext(ctx); ok {
		accountID = lease.Account().ID
	}
	attempt := &physicalAttempt{ctx: ctx, anti: anti, channel: generationRPCChannel(rpc.Method), value: GenerationAttempt{
		ID: fmt.Sprintf("%s/attempt-%d", request.ID, generationAttemptSequence.Add(1)), RequestID: request.ID,
		RequestedModel: requested, BaseModel: base, RequestedChannel: request.Selection.Channel,
		AntiTruncation: request.Selection.AntiTruncation, AccountID: accountID, Method: rpc.Method, Mode: mode,
		Reason: reason, ContinuationRound: request.continuationRound, Stage: "preparing", StartedAt: now, UsageSource: "unknown",
	}}
	attempt.update(func(*GenerationAttempt) {})
	return context.WithValue(ctx, physicalAttemptKey{}, attempt), attempt
}

func (attempt *physicalAttempt) update(change func(*GenerationAttempt)) {
	if attempt == nil {
		return
	}
	attempt.mu.Lock()
	change(&attempt.value)
	attempt.value.Revision++
	attempt.value.DurationMS = time.Since(attempt.value.StartedAt).Milliseconds()
	snapshot := attempt.value
	if snapshot.Usage != nil {
		copied := *snapshot.Usage
		snapshot.Usage = &copied
	}
	attempt.mu.Unlock()
	attempt.anti.attempt(snapshot)
	reportGenerationAttempt(attempt.ctx, snapshot)
}

// Mark immediately before the native send. This is an attempted send, not proof
// that Google executed or billed it. Receiving headers is tracked separately.
func markPhysicalSend(ctx context.Context) {
	attempt, _ := ctx.Value(physicalAttemptKey{}).(*physicalAttempt)
	if attempt == nil {
		return
	}
	attempt.update(func(value *GenerationAttempt) {
		value.Sent = true
		value.Channel = attempt.channel
		value.Stage = "sending"
	})
}

func (attempt *physicalAttempt) attach(response *RPCResponse) *RPCResponse {
	if attempt == nil {
		return response
	}
	if response == nil || response.Body == nil {
		attempt.fail(errors.New("empty upstream response"))
		return response
	}
	attempt.update(func(value *GenerationAttempt) {
		value.ResponseReceived = true
		value.StatusCode = response.StatusCode
		value.Stage = "response"
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			attempt.terminal = true
			value.Stage = "http_error"
			value.Error = fmt.Sprintf("upstream HTTP %d", response.StatusCode)
		}
	})
	response.attempt = attempt
	response.Body = &physicalAttemptBody{ReadCloser: response.Body, attempt: attempt}
	return response
}

func (attempt *physicalAttempt) fail(err error) {
	if attempt == nil || err == nil {
		return
	}
	attempt.update(func(value *GenerationAttempt) {
		// A later local projection failure cannot rewrite an already observed
		// upstream completion into a fictitious extra failed physical request.
		var rpcFailure *RPCError
		var protocolFailure *ProtocolEvidenceError
		upstreamFailure := errors.As(err, &rpcFailure) || errors.As(err, &protocolFailure)
		if attempt.terminal && !(value.Stage == "upstream_finished" && upstreamFailure) {
			return
		}
		attempt.terminal = true
		value.Stage = "failed"
		if !value.Sent {
			value.Stage = "preparation_failed"
		}
		var rpcError *RPCError
		switch {
		case errors.Is(err, context.Canceled):
			value.Stage = "cancelled"
			value.Error = "context cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			value.Stage = "timeout"
			value.Error = "context deadline exceeded"
		case errors.As(err, &rpcError):
			value.Error = fmt.Sprintf("upstream RPC status=%d code=%d", rpcError.StatusCode, rpcError.Code)
		default:
			value.Error = fmt.Sprintf("%T", err)
		}
	})
}

func (attempt *physicalAttempt) observe(event Event) {
	if attempt == nil {
		return
	}
	attempt.anti.raw(attempt.value.ID, event)
	switch event.Kind {
	case EventUsage:
		if event.Usage != nil {
			copied := *event.Usage
			copied.Source = "upstream"
			attempt.update(func(value *GenerationAttempt) { value.Usage = &copied; value.UsageSource = "upstream" })
		}
	case EventFinish:
		attempt.update(func(value *GenerationAttempt) {
			if !attempt.terminal {
				attempt.terminal = true
				value.Stage = "upstream_finished"
			}
		})
	case EventError:
		attempt.fail(event.Err)
	}
}

func (attempt *physicalAttempt) decoded(err error, continued bool) {
	if attempt == nil {
		return
	}
	attempt.anti.decoded(attempt.value.ID, err, continued)
	if errors.Is(err, errStopSequenceMatched) {
		attempt.update(func(value *GenerationAttempt) {
			if !attempt.terminal {
				attempt.terminal = true
				value.Stage = "client_stop"
			}
		})
		return
	}
	if err != nil {
		attempt.fail(err)
		return
	}
	if continued {
		attempt.update(func(value *GenerationAttempt) {
			if !attempt.terminal {
				attempt.terminal = true
				value.Stage = "continued"
			}
		})
	}
}

type physicalAttemptBody struct {
	io.ReadCloser
	attempt  *physicalAttempt
	once     sync.Once
	closeErr error
}

func (body *physicalAttemptBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		body.attempt.fail(err)
	}
	return n, err
}

func (body *physicalAttemptBody) Close() error {
	body.once.Do(func() {
		body.closeErr = body.ReadCloser.Close()
		if err := body.attempt.ctx.Err(); err != nil {
			body.attempt.fail(err)
		} else if body.closeErr != nil {
			body.attempt.fail(body.closeErr)
		}
		body.attempt.update(func(value *GenerationAttempt) {
			if !body.attempt.terminal {
				value.Stage = "closed"
			}
		})
	})
	return body.closeErr
}
