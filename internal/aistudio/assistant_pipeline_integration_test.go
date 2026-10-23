package aistudio

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Unlike API adapter fixtures, these exercise the real Client.Generate loop,
// lease policy, transport codec and usage projection. All RPC bytes are synthetic.
func assistantCoreFixture(t *testing.T) (*assistantFixture, context.Context, *AccountLease) {
	t.Helper()
	fixture := newAssistantFixture(t)
	pool, account := assistantPolicyFixture(t)
	selection := AccountSelection{ModelID: "gemini-4-argon", Method: "generateContent", Channel: ChannelAssistant, StrictChannel: true}
	lease, _, err := pool.TryAcquireFor(context.Background(), selection)
	if err != nil || lease == nil {
		t.Fatalf("assistant fixture lease: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	})
	model := account.Models[0]
	fixture.client.catalogs[account.ID] = modelCatalog{models: []Model{model}, entries: map[string]modelEntry{model.ID: {model: model, defaults: GenerationDefaults{MaxOutputTokens: 32768, ContinuedOutputTokens: 262144}}}}
	ctx := ContextWithAccountLease(fixture.ctx, lease)
	return fixture, ctx, lease
}

func assistantCoreRequest(lease *AccountLease) GenerateRequest {
	return GenerateRequest{ID: "fixture-assistant-core", Model: "assistant/gemini-4-argon", AccountID: lease.Account().ID, Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture question"}}}}}
}

func TestAssistantCoreGenerationUsesAssistantAndRawUsage(t *testing.T) {
	for _, unary := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "unary"}[unary], func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("hidden thought", true), assistantPart("fixture answer", false))))
			request := assistantCoreRequest(lease)
			request.Unary = unary
			request.Config.HideThinking = true
			source, err := fixture.client.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for event := range source {
				events = append(events, event)
			}
			if pipelineError(events) != nil || pipelineText(events) != "fixture answer" || pipelineFinish(events) == nil {
				t.Fatalf("projection: %+v", events)
			}
			usage := pipelineUsage(events)
			if usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
				t.Fatalf("usage: %+v", usage)
			}
			for _, event := range events {
				if event.Channel != ChannelAssistant || event.ProviderModel != "gemini-4-argon" || event.Kind == EventReasoning {
					t.Fatalf("identity/thought leak: %+v", event)
				}
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			aux, protected, attempts := fixture.snapshot()
			if len(aux) != 3 || len(protected) != 1 || len(attempts) != 1 {
				t.Fatalf("control/generation accounting: %d/%d/%d", len(aux), len(protected), len(attempts))
			}
			for _, attempt := range attempts {
				if attempt.Channel != ChannelAssistant || attempt.Method != assistantInitMethod || attempt.Stage != "upstream_finished" || attempt.UsageSource != "upstream" {
					t.Fatalf("attempt: %+v", attempt)
				}
			}
		})
	}
}

func TestAssistantCoreUnsupportedContractsFailBeforeResources(t *testing.T) {
	for _, kind := range []string{"anti", "max_tokens", "media", "code_history"} {
		t.Run(kind, func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			request := assistantCoreRequest(lease)
			switch kind {
			case "code_history":
				request.Contents = append(request.Contents, Content{Role: RoleAssistant, Parts: []Part{{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "fixture"}}}})
			case "anti":
				request.Model = "assistant/anti-truncation/gemini-4-argon"
			case "max_tokens":
				budget := int64(128)
				request.Config.MaxOutputTokens = &budget
			case "media":
				request.Contents[0].Parts = append(request.Contents[0].Parts, Part{InlineData: &Blob{MIME: "image/png", Data: []byte("fixture")}})
			}
			_, err := fixture.client.Generate(ctx, request)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("unimplemented contract accepted: %v", err)
			}
			if kind == "anti" || kind == "code_history" {
				if !strings.Contains(err.Error(), "mapping not implemented") {
					t.Fatalf("misreported upstream unsupported: %v", err)
				}
			}
			aux, protected, attempts := fixture.snapshot()
			if len(aux) != 0 || len(protected) != 0 || len(attempts) != 0 {
				t.Fatal("invalid request created resources or generation")
			}
		})
	}
}

func TestAssistantCoreMissingCompletionDoesNotReplayOrFinish(t *testing.T) {
	fixture, ctx, lease := assistantCoreFixture(t)
	fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 2, nil, assistantPart("partial", false))))
	source, err := fixture.client.Generate(ctx, assistantCoreRequest(lease))
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for event := range source {
		events = append(events, event)
	}
	if pipelineError(events) == nil || pipelineFinish(events) != nil {
		t.Fatalf("missing finish laundered: %+v", events)
	}
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
	_, protected, attempts := fixture.snapshot()
	if len(protected) != 1 || len(attempts) != 1 {
		t.Fatal("decoding failure generated again")
	}
}

func TestAssistantCoreStopUsesLocalProjectionNotExtraGeneration(t *testing.T) {
	fixture, ctx, lease := assistantCoreFixture(t)
	fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("before STOP after", false))))
	request := assistantCoreRequest(lease)
	request.Config.StopSequences = []string{" STOP"}
	source, err := fixture.client.Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for event := range source {
		events = append(events, event)
	}
	if pipelineError(events) != nil || pipelineFinish(events) == nil || pipelineText(events) != "before" {
		t.Fatalf("stop: %+v", events)
	}
	if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
		t.Fatalf("stop discarded known usage: %+v", usage)
	}
	fixture.waitCleanup(t, assistantCancelMethod)
	fixture.waitCleanup(t, assistantDeleteAppletMethod)
	_, protected, attempts := fixture.snapshot()
	if len(protected) != 1 || len(attempts) != 1 {
		t.Fatal("stop replayed")
	}
}
