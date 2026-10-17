package aistudio

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

func TestPlaygroundContinuationKeepsPinAndPhysicalUsage(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelPlayground)
	budget := int64(65536)
	fixture.reply = func(request GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
		if request.Selection.Channel != ChannelPlayground || rpc.Method != "GenerateContent" {
			t.Fatal("continuation escaped PG")
		}
		if index == 1 {
			return fixtureResponse(t, rpc, []Part{{Text: "first"}}, "continue", &Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}, "fixture-continuation"), nil
		}
		if index != 2 || request.continuationRound != 1 || request.continuationToken != "fixture-continuation" {
			t.Fatalf("continuation state lost: round=%d index=%d", request.continuationRound, index)
		}
		return fixtureResponse(t, rpc, []Part{{Text: "second"}}, "stop", &Usage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}, ""), nil
	}
	events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}})
	if pipelineError(events) != nil || pipelineText(events) != "firstsecond" || pipelineFinish(events) == nil {
		t.Fatalf("bad continuation: %+v", events)
	}
	if usage := pipelineUsage(events); usage == nil || usage.OutputTokens != 7 || usage.TotalTokens != 27 {
		t.Fatalf("logical usage not merged by original rule: %+v", usage)
	}
	if len(fixture.sends) != 2 || len(fixture.attempts) != 2 {
		t.Fatalf("physical attempts %d/%d", len(fixture.sends), len(fixture.attempts))
	}
	byRound := map[int]GenerationAttempt{}
	for _, attempt := range fixture.attempts {
		byRound[attempt.ContinuationRound] = attempt
		if attempt.Channel != ChannelPlayground || attempt.UsageSource != "upstream" {
			t.Fatalf("bad attempt: %+v", attempt)
		}
	}
	if byRound[0].Usage == nil || byRound[0].Usage.TotalTokens != 13 || byRound[1].Usage == nil || byRound[1].Usage.TotalTokens != 24 {
		t.Fatalf("per-send usage replaced with aggregate: %+v", byRound)
	}
}

func TestPlaygroundContinuationMissingTokenDoesNotGenerateAgain(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelPlayground)
	budget := int64(65536)
	fixture.reply = func(_ GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
		return fixtureResponse(t, rpc, []Part{{Text: "partial"}}, "continue", &Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}, ""), nil
	}
	events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}})
	if pipelineError(events) == nil || pipelineFinish(events) != nil || len(fixture.sends) != 1 {
		t.Fatalf("missing token was repaired/retried: %+v", events)
	}
	if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 13 {
		t.Fatal("known partial usage hidden")
	}
}

func TestPlaygroundContinuationQuotaFailurePreservesEarlierUsage(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelPlayground)
	budget := int64(65536)
	fixture.reply = func(_ GenerateRequest, rpc RPCRequest, index int) ([]byte, error) {
		if index == 1 {
			return fixtureResponse(t, rpc, []Part{{Text: "partial"}}, "continue", &Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}, "fixture-continuation"), nil
		}
		return nil, &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "fixture quota"}
	}
	events := fixture.generate(t, GenerateRequest{Model: "playground/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}})
	if pipelineError(events) == nil || pipelineFinish(events) != nil || len(fixture.sends) != 2 {
		t.Fatalf("late quota became success/replay: %+v", events)
	}
	if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 13 {
		t.Fatal("earlier known usage lost")
	}
	for _, attempt := range fixture.attempts {
		if attempt.ContinuationRound == 1 && (attempt.Usage != nil || attempt.UsageSource != "unknown" || attempt.Stage != "failed") {
			t.Fatalf("unknown failed-send usage fabricated: %+v", attempt)
		}
	}
}

func TestStrictBuildRejectsBudgetAboveNativeLimitBeforeSend(t *testing.T) {
	fixture := newPipelineFixture(t, ChannelBuild)
	budget := int64(65537)
	fixture.reply = func(GenerateRequest, RPCRequest, int) ([]byte, error) {
		t.Fatal("incompatible Build request sent")
		return nil, nil
	}
	_, err := fixture.client.Generate(fixture.ctx, GenerateRequest{ID: t.Name(), AccountID: fixture.account.ID, Model: "build/fixture-model", Config: GenerationConfig{MaxOutputTokens: &budget}, Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture"}}}}})
	if !errors.Is(err, ErrInvalidArgument) || len(fixture.sends) != 0 {
		t.Fatalf("budget borrowed PG continuation: %v", err)
	}
}

func TestPhysicalPreparationFailureIsNotASend(t *testing.T) {
	var mu sync.Mutex
	var snapshots []GenerationAttempt
	ctx := ContextWithGenerationAttemptObserver(context.Background(), func(value GenerationAttempt) { mu.Lock(); snapshots = append(snapshots, value); mu.Unlock() })
	request, _ := NormalizeGenerateRequestModel(GenerateRequest{ID: t.Name(), Model: "playground/fixture-model"})
	// The final RPC guard rejects Build before any WAA preparer is used.
	_, err := (&WorkerProtectedTransport{}).DoProtected(ctx, request, RPCRequest{Method: buildProxyUnaryMethod})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("guard failed: %v", err)
	}
	if len(snapshots) < 2 {
		t.Fatal("preparation evidence absent")
	}
	last := snapshots[len(snapshots)-1]
	if last.Sent || last.ResponseReceived || last.Usage != nil || last.UsageSource != "unknown" || last.Stage != "preparation_failed" {
		t.Fatalf("preparation counted as generation: %+v", last)
	}
}

func TestPhysicalLateProtocolFailureOverridesCompletionWithoutLosingUsage(t *testing.T) {
	var last GenerationAttempt
	ctx := ContextWithGenerationAttemptObserver(context.Background(), func(value GenerationAttempt) { last = value })
	request, _ := NormalizeGenerateRequestModel(GenerateRequest{ID: t.Name(), Model: "build/fixture-model"})
	ctx, attempt := beginPhysicalAttempt(ctx, request, RPCRequest{Method: buildProxyStreamedMethod, Streaming: true})
	markPhysicalSend(ctx)
	attempt.observe(Event{Kind: EventUsage, Usage: &Usage{TotalTokens: 17}})
	attempt.observe(Event{Kind: EventFinish, FinishReason: "stop"})
	attempt.decoded(&RPCError{Method: buildProxyStreamedMethod, StatusCode: 429, Code: 8, Message: "must not copy raw body"}, false)
	if last.Stage != "failed" || last.Usage == nil || last.Usage.TotalTokens != 17 || last.Error != "upstream RPC status=429 code=8" {
		t.Fatalf("late failure laundered: %+v", last)
	}
}
