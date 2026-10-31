package aistudio

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
)

func samplingFloat(v float64) *float64 { return &v }
func samplingInt(v int) *int           { return &v }
func samplingSeed(v int64) *int64      { return &v }

func TestAssistantSamplingPurePreparation(t *testing.T) {
	cases := []GenerationConfig{
		{}, {Temperature: samplingFloat(0)}, {Temperature: samplingFloat(2)},
		{TopP: samplingFloat(0)}, {TopP: samplingFloat(1)},
		{TopK: samplingInt(0)}, {TopK: samplingInt(1000000)},
		{Seed: samplingSeed(-1)}, {Seed: samplingSeed(0)}, {Seed: samplingSeed(math.MinInt64)}, {Seed: samplingSeed(math.MaxInt64)},
		{Temperature: samplingFloat(0.8), TopP: samplingFloat(0.9), TopK: samplingInt(50), Seed: samplingSeed(7)},
	}
	for index, config := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			request := assistantDowngradeRequest()
			config.ReasoningEffort, config.StopSequences, config.HideThinking = "high", []string{"stop fixture"}, true
			config.ThinkingBudget = samplingSeed(2048)
			request.Config = config
			before := assistantDowngradeFingerprintOf(t, request)
			got, err := prepareAssistantTextRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			want := config
			want.Temperature, want.TopP, want.TopK, want.Seed = nil, nil, nil, nil
			if !reflect.DeepEqual(got.Config, want) {
				t.Fatalf("sampling not cleared or unrelated fields changed: %+v", got.Config)
			}
			if assistantDowngradeFingerprintOf(t, request) != before || !reflect.DeepEqual(request.Config, config) {
				t.Fatal("caller input/pointer target changed")
			}
			again, err := prepareAssistantTextRequest(got)
			if err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("not idempotent: %v", err)
			}
		})
	}
}

func TestAssistantSamplingInvalidBeforeResources(t *testing.T) {
	cases := []GenerationConfig{
		{Temperature: samplingFloat(-0.1)}, {Temperature: samplingFloat(2.1)}, {Temperature: samplingFloat(math.NaN())}, {Temperature: samplingFloat(math.Inf(1))}, {Temperature: samplingFloat(math.Inf(-1))},
		{TopP: samplingFloat(-0.1)}, {TopP: samplingFloat(1.1)}, {TopP: samplingFloat(math.NaN())}, {TopP: samplingFloat(math.Inf(1))}, {TopP: samplingFloat(math.Inf(-1))},
		{TopK: samplingInt(-1)}, {Temperature: samplingFloat(0.8), TopP: samplingFloat(1.1), Seed: samplingSeed(0)}, {Temperature: samplingFloat(2.1), TopP: samplingFloat(0.5)},
	}
	for index, config := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			request := assistantCoreRequest(lease)
			request.Config = config
			if _, err := fixture.client.Generate(ctx, request); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("invalid sampling accepted: %v", err)
			}
			aux, sends, attempts := fixture.snapshot()
			if len(aux)+len(sends)+len(attempts) != 0 {
				t.Fatal("invalid sampling created resources/submitted")
			}
			// Check pointer identity and targets even for NaN (which is not DeepEqual to itself).
			if request.Config.Temperature != config.Temperature || request.Config.TopP != config.TopP || request.Config.TopK != config.TopK || request.Config.Seed != config.Seed {
				t.Fatal("input pointer changed")
			}
		})
	}
}

func TestAssistantSamplingCodecAndClientEquivalent(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			request := assistantTestRequest()
			request.Model, request.Selection.BaseModel = model, model
			plain, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request")
			if err != nil {
				t.Fatal(err)
			}
			request.Config.Temperature, request.Config.TopP, request.Config.TopK, request.Config.Seed = samplingFloat(0), samplingFloat(1), samplingInt(0), samplingSeed(math.MinInt64)
			if err := validateAssistantRequest(request); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("defensive validator accepted leftovers")
			}
			wire, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request")
			if err != nil || !reflect.DeepEqual(plain, wire) {
				t.Fatalf("sampling changed wire: %v", err)
			}
		})
	}
	for _, unary := range []bool{false, true} {
		t.Run(fmt.Sprint(unary), func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("fixture complete text", false))))
			request := assistantCoreRequest(lease)
			request.Unary = unary
			request.Config = GenerationConfig{Temperature: samplingFloat(0.8), TopP: samplingFloat(0.9), TopK: samplingInt(40), Seed: samplingSeed(0), MaxOutputTokens: samplingSeed(1)}
			stream, err := fixture.client.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for event := range stream {
				events = append(events, event)
			}
			if pipelineError(events) != nil || pipelineFinish(events) == nil || pipelineFinish(events).FinishReason != "stop" || pipelineUsage(events) == nil {
				t.Fatalf("sampling changed terminal/usage: %+v", events)
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			_, sends, attempts := fixture.snapshot()
			if len(sends) != 1 || len(attempts) != 1 || *request.Config.Seed != 0 {
				t.Fatal("replayed submit/input changed")
			}
		})
	}
}

func TestAssistantSamplingConcurrentAndOtherRoutes(t *testing.T) {
	var wg sync.WaitGroup
	for index := range 32 {
		wg.Go(func() {
			request := assistantDowngradeRequest()
			request.Config = GenerationConfig{Temperature: samplingFloat(0.8), TopP: samplingFloat(0.9), TopK: samplingInt(index), Seed: samplingSeed(int64(index))}
			for _, channel := range []Channel{"", ChannelPlayground, ChannelBuild, ChannelAssistant} {
				request.Selection.Channel = channel
				got, err := prepareAssistantTextRequest(request)
				if err != nil {
					t.Error(err)
					return
				}
				if channel == ChannelAssistant {
					if got.Config.Temperature != nil || got.Config.TopP != nil || got.Config.TopK != nil || got.Config.Seed != nil {
						t.Error("sampling not cleared")
					}
				} else if !reflect.DeepEqual(got, request) || got.Config.Seed != request.Config.Seed {
					t.Error("changed unrelated route")
				}
			}
			if *request.Config.Seed != int64(index) || *request.Config.TopK != index {
				t.Error("cross-request state")
			}
		})
	}
	wg.Wait()
}

func TestAssistantSamplingCrossRouteEncoding(t *testing.T) {
	config := GenerationConfig{Temperature: samplingFloat(0.8), TopP: samplingFloat(0.9), TopK: samplingInt(40), Seed: samplingSeed(-7)}
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, anti := range []bool{false, true} {
			request := assistantTestRequest()
			request.Selection.Channel, request.Selection.AntiTruncation, request.Config = channel, anti, config
			got, err := prepareAssistantTextRequest(request)
			if err != nil || !reflect.DeepEqual(got.Config, config) {
				t.Fatalf("changed route=%s anti=%t: %v", channel, anti, err)
			}
			if channel == ChannelPlayground {
				wire, err := encodeGenerationConfig(got.Config, GenerationDefaults{MaxOutputTokens: 32768})
				if err != nil || wire[4] != 0.8 || wire[5] != 0.9 || wire[6] != 40 || wire[18] != int64(-7) {
					t.Fatalf("PG sampling no longer encoded: %v %v", wire, err)
				}
			} else {
				wire, err := encodeBuildGenerationConfig(got.Config, GenerationDefaults{MaxOutputTokens: 32768})
				if err != nil || wire["temperature"] != 0.8 || wire["topP"] != 0.9 || wire["topK"] != 40 || wire["seed"] != int64(-7) {
					t.Fatalf("Build sampling no longer encoded: %v %v", wire, err)
				}
			}
		}
	}
	bad := config
	bad.TopP = samplingFloat(1.1)
	if _, err := encodeGenerationConfig(bad, GenerationDefaults{MaxOutputTokens: 32768}); err == nil {
		t.Fatal("PG invalid boundary changed")
	}
	if _, err := encodeBuildGenerationConfig(bad, GenerationDefaults{MaxOutputTokens: 32768}); err == nil {
		t.Fatal("Build invalid boundary changed")
	}
}

func TestAssistantSamplingOtherRestrictionsAndValidation(t *testing.T) {
	for _, mode := range []string{"budget_zero", "budget_negative", "media", "schema", "reasoning", "code", "unknown_tool", "anti", "lease_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			request := assistantCoreRequest(lease)
			request.Config = GenerationConfig{Temperature: samplingFloat(0.8), TopP: samplingFloat(0.9), TopK: samplingInt(40), Seed: samplingSeed(0)}
			switch mode {
			case "budget_zero":
				request.Config.MaxOutputTokens = samplingSeed(0)
			case "budget_negative":
				request.Config.MaxOutputTokens = samplingSeed(-1)
			case "media":
				request.Contents[0].Parts = []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte("fixture")}}}
			case "schema":
				request.Config.ResponseSchema = []byte(`{"type":"object"}`)
			case "reasoning":
				request.Config.ReasoningEffort = "not-an-effort"
			case "code":
				request.Contents[0].Parts = []Part{{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "fixture"}}}
			case "unknown_tool":
				request.Tools.Google = []string{"unknown-fixture-tool"}
			case "anti":
				request.Model = "assistant/anti-truncation/gemini-4-argon"
			case "lease_mismatch":
				request.Model = "playground/gemini-4-argon"
			}
			if _, err := fixture.client.Generate(ctx, request); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("sampling bypassed %s: %v", mode, err)
			}
			aux, sends, attempts := fixture.snapshot()
			if len(aux)+len(sends)+len(attempts) != 0 {
				t.Fatal("rejected input created resource/submit")
			}
		})
	}
	fixture, _, lease := assistantCoreFixture(t)
	pool := NewAccountPool([]*Account{lease.Account()}, 1)
	pool.SetUpstreamChannels([]Channel{ChannelAssistant})
	pooled, err := NewPooledService(pool, fixture.client)
	if err != nil {
		t.Fatal(err)
	}
	request := assistantCoreRequest(lease)
	request.Config = GenerationConfig{Temperature: samplingFloat(0.8), TopP: samplingFloat(0.9), TopK: samplingInt(40), Seed: samplingSeed(0)}
	if err := pooled.ValidateGenerateRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Config.TopP = samplingFloat(1.1)
	if err := pooled.ValidateGenerateRequest(request); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("pre-lease validation swallowed invalid sampling")
	}
	aux, sends, attempts := fixture.snapshot()
	if len(aux)+len(sends)+len(attempts) != 0 {
		t.Fatal("static validation accessed generation resources")
	}
}
