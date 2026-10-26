package aistudio

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestAssistantOutputBudgetPurePreparation(t *testing.T) {
	for _, budget := range []int64{1, 128, 32769, 262145, math.MaxInt64} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			request := assistantDowngradeRequest()
			request.Config.MaxOutputTokens = &budget
			thinking := int64(2048)
			request.Config.ThinkingBudget = &thinking
			request.Config.ReasoningEffort = "high"
			request.Config.StopSequences = []string{"fixture stop"}
			before := assistantDowngradeFingerprintOf(t, request)
			prepared, err := prepareAssistantTextRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Config.MaxOutputTokens != nil {
				t.Fatal("Assistant must ignore the legal output budget")
			}
			want := request.Config
			want.MaxOutputTokens = nil
			if !reflect.DeepEqual(prepared.Config, want) || prepared.Model != request.Model || prepared.Selection != request.Selection {
				t.Fatal("unrelated thinking/stop/identity configuration changed")
			}
			if assistantDowngradeFingerprintOf(t, request) != before || *request.Config.MaxOutputTokens != budget {
				t.Fatal("caller input or pointer target mutated")
			}
			again, err := prepareAssistantTextRequest(prepared)
			if err != nil || !reflect.DeepEqual(prepared, again) {
				t.Fatalf("non-idempotent preparation: %v", err)
			}
		})
	}
	request := assistantDowngradeRequest()
	prepared, err := prepareAssistantTextRequest(request)
	if err != nil || prepared.Config.MaxOutputTokens != nil {
		t.Fatalf("omitted budget must remain omitted: %v", err)
	}
	for _, budget := range []int64{0, -1, math.MinInt64} {
		request.Config.MaxOutputTokens = &budget
		if _, err := prepareAssistantTextRequest(request); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid budget %d accepted: %v", budget, err)
		}
	}
}

func TestAssistantOutputBudgetOtherChannelsUnchanged(t *testing.T) {
	for _, channel := range []Channel{"", ChannelPlayground, ChannelBuild} {
		for _, budget := range []int64{-1, 0, 1, math.MaxInt64} {
			request := assistantDowngradeRequest()
			request.Selection.Channel = channel
			request.Config.MaxOutputTokens = &budget
			prepared, err := prepareAssistantTextRequest(request)
			if err != nil || !reflect.DeepEqual(prepared, request) || prepared.Config.MaxOutputTokens != request.Config.MaxOutputTokens {
				t.Fatalf("changed channel=%q budget=%d: %v", channel, budget, err)
			}
		}
	}
}

func TestAssistantOutputBudgetConcurrentPreparation(t *testing.T) {
	var wg sync.WaitGroup
	for index := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			budget := int64(index + 1)
			request := assistantDowngradeRequest()
			request.Config.MaxOutputTokens = &budget
			prepared, err := prepareAssistantTextRequest(request)
			if err != nil || prepared.Config.MaxOutputTokens != nil || budget != int64(index+1) {
				t.Errorf("concurrent preparation leaked state: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestAssistantOutputBudgetCodecEquivalentAndDefensiveValidator(t *testing.T) {
	request := assistantTestRequest() // Direct codec's existing normalized Selection contract.
	request.Config.ReasoningEffort = "low"
	plain, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request")
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(1)
	request.Config.MaxOutputTokens = &budget
	if err := validateAssistantRequest(request); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("raw validator must still reject unprepared leftovers")
	}
	wire, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request")
	if err != nil || !reflect.DeepEqual(plain, wire) {
		t.Fatalf("legal budget changed Assistant wire: %v", err)
	}
	if request.Config.MaxOutputTokens != &budget || budget != 1 {
		t.Fatal("codec mutated original request")
	}
	request.Selection = ModelSelection{}
	request.Model = "assistant/gemini-4-argon"
	if _, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("08 must not silently add normalization to raw direct codec calls")
	}
}

func TestAssistantOutputBudgetRealClientDoesNotTruncate(t *testing.T) {
	for _, unary := range []bool{false, true} {
		t.Run(fmt.Sprint(unary), func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("a complete synthetic answer much longer than one token", false))))
			request := assistantCoreRequest(lease)
			budget := int64(1)
			request.Config.MaxOutputTokens = &budget
			request.Unary = unary
			source, err := fixture.client.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for event := range source {
				events = append(events, event)
			}
			finish := pipelineFinish(events)
			if pipelineError(events) != nil || finish == nil || finish.FinishReason != "stop" || pipelineText(events) != "a complete synthetic answer much longer than one token" {
				t.Fatalf("local budget truncated or changed termination: %+v", events)
			}
			usage := pipelineUsage(events)
			if usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
				t.Fatalf("budget fabricated usage: %+v", usage)
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			aux, sends, attempts := fixture.snapshot()
			if len(aux) != 3 || len(sends) != 1 || len(attempts) != 1 || *request.Config.MaxOutputTokens != 1 {
				t.Fatalf("replayed generation or mutated input: %d/%d/%d", len(aux), len(sends), len(attempts))
			}
		})
	}
}

func TestAssistantOutputBudgetInvalidAndOtherRestrictionsBeforeResources(t *testing.T) {
	for _, kind := range []string{"zero", "negative", "temperature", "schema", "media", "code", "reasoning", "unknown_google"} {
		t.Run(kind, func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			request := assistantCoreRequest(lease)
			budget := int64(1)
			request.Config.MaxOutputTokens = &budget
			switch kind {
			case "zero":
				budget = 0
			case "negative":
				budget = -1
			case "temperature":
				v := 0.1
				request.Config.Temperature = &v
			case "schema":
				request.Config.ResponseSchema = []byte(`{"type":"object"}`)
			case "media":
				request.Contents[0].Parts = []Part{{InlineData: &Blob{MIME: "image/png", Data: []byte("fixture")}}}
			case "code":
				request.Contents[0].Parts = []Part{{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "fixture"}}}
			case "reasoning":
				request.Config.ReasoningEffort = "not-an-effort"
			case "unknown_google":
				request.Tools.Google = []string{"unknown-fixture-tool"}
			}
			if _, err := fixture.client.Generate(ctx, request); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("invalid request accepted: %v", err)
			}
			aux, sends, attempts := fixture.snapshot()
			if len(aux)+len(sends)+len(attempts) != 0 {
				t.Fatal("rejected request created resources or generation")
			}
		})
	}
}
