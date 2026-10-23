package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func assistantTextHistoryFixture() []Content {
	return []Content{
		{Role: RoleUser, Parts: []Part{{Text: "fixture request"}}},
		{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "fixture-call", Name: "read_fixture", Arguments: json.RawMessage(`{"path":"fixture.txt"}`)}}}},
		{Role: RoleTool, Parts: []Part{{FunctionResult: &FunctionResult{ID: "fixture-call", Content: json.RawMessage(`{"text":"fixture result"}`)}}}},
	}
}

func TestAssistantTextDowngradeRealPipelineAndPooledValidation(t *testing.T) {
	for _, mode := range []string{"auto", "none", "required", "named"} {
		t.Run(mode, func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("fixture text answer", false))))
			request := assistantCoreRequest(lease)
			request.Contents = assistantTextHistoryFixture()
			parallel := false
			request.Tools = Tools{Functions: []FunctionDeclaration{{Name: "read_fixture", Parameters: json.RawMessage(`{"type":"object"}`)}}, ToolConfig: ToolConfig{Mode: mode, ParallelCalls: &parallel}}
			if mode == "named" {
				request.Tools.ToolConfig.Mode = "required"
				request.Tools.ToolConfig.AllowedFunctionNames = []string{"read_fixture"}
			}
			before, _ := json.Marshal(request)
			service, err := NewPooledService(lease.pool, fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.ValidateGenerateRequest(request); err != nil {
				t.Fatalf("prelease validation blocked downgrade: %v", err)
			}
			source, err := service.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for event := range source {
				events = append(events, event)
				if event.Kind == EventToolCall {
					t.Fatal("text path emitted a native tool event")
				}
			}
			if pipelineError(events) != nil || pipelineFinish(events) == nil || pipelineText(events) != "fixture text answer" {
				t.Fatalf("pipeline: %+v", events)
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			_, sends, attempts := fixture.snapshot()
			if len(sends) != 1 || len(attempts) != 1 {
				t.Fatal("unexpected attempts")
			}
			var wire []json.RawMessage
			if err := json.Unmarshal(sends[0].Body, &wire); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(wire[0]), `functionResponse`) || !strings.Contains(string(wire[0]), `read_fixture`) {
				t.Fatalf("transcript lost: %s", wire[0])
			}
			var turns []json.RawMessage
			if err := json.Unmarshal(wire[0], &turns); err != nil {
				t.Fatal(err)
			}
			for _, turnRaw := range turns {
				var turn []json.RawMessage
				_ = json.Unmarshal(turnRaw, &turn)
				var parts [][]json.RawMessage
				_ = json.Unmarshal(turn[0], &parts)
				for _, part := range parts {
					for _, slot := range []int{10, 11, 14} {
						if !isJSONNull(rawAt(part, slot)) {
							t.Fatalf("native tool/signature leaked: %s", turnRaw)
						}
					}
				}
			}
			after, _ := json.Marshal(request)
			if string(after) != string(before) {
				t.Fatal("pipeline mutated original request")
			}
		})
	}
}

func TestAssistantTextDowngradeDoesNotWeakenStrictOtherChannels(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		t.Run(string(channel), func(t *testing.T) {
			request := GenerateRequest{Selection: ModelSelection{Channel: channel}, Tools: Tools{Functions: []FunctionDeclaration{{Name: "required_fixture"}}, ToolConfig: ToolConfig{Mode: "required"}}, Contents: assistantTextHistoryFixture()}
			got, err := prepareAssistantTextRequest(request)
			if err != nil || !reflect.DeepEqual(got, request) {
				t.Fatal("changed other route")
			}
			entry := modelEntry{model: Model{ID: "fixture", Capabilities: map[string]bool{}}}
			if err := validateStrictGenerateEntry(got, entry); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("strict tools weakened: %v", err)
			}
		})
	}
}

func TestAssistantTextDowngradeRejectsAntiBeforeLeaseOrSend(t *testing.T) {
	fixture, _, lease := assistantCoreFixture(t)
	service, _ := NewPooledService(lease.pool, fixture.client)
	request := assistantCoreRequest(lease)
	request.Model = "assistant/anti-truncation/gemini-4-argon"
	for i, err := range []error{service.ValidateGenerateRequest(request), func() error { _, err := service.Generate(context.Background(), request); return err }()} {
		if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "anti-truncation") {
			t.Fatalf("%d anti became plain: %v", i, err)
		}
	}
	aux, sends, attempts := fixture.snapshot()
	if len(aux)+len(sends)+len(attempts) != 0 {
		t.Fatal(fmt.Sprint("anti caused resources ", aux))
	}
}
