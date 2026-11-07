package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestGeneratedOutputPartsTextBoundaries(t *testing.T) {
	call := &FunctionCall{ID: "tool", Name: "lookup", Arguments: json.RawMessage(`{"x":1}`)}
	code := &ExecutableCode{Language: "PYTHON", Code: "print(1)"}
	result := &CodeExecutionResult{Outcome: "OK", Output: "1"}
	var output generatedOutputParts
	for _, event := range []Event{
		{Kind: EventText, Text: "你", ThoughtSignature: "v"},
		{Kind: EventReasoning, Text: "思", ThoughtSignature: "r"},
		{Kind: EventText, ThoughtSignature: "ignored"},
		{Kind: EventToolCall},
		{Kind: EventMedia, Media: &Media{}},
		{Kind: EventText, Text: "😀", ThoughtSignature: "v"},
		{Kind: EventReasoning, Text: "考", ThoughtSignature: "r"},
		{Kind: EventText, Text: "x", ThoughtSignature: "v2"},
		{Kind: EventText, Text: "y", ThoughtSignature: "v2"},
		{Kind: EventToolCall, ToolCall: call},
		{Kind: EventText, Text: "a"},
		{Kind: EventMedia, Media: &Media{MIME: "image/png", Data: []byte{1}}},
		{Kind: EventText, Text: "b"},
		{Kind: EventExecutableCode, ExecutableCode: code},
		{Kind: EventText, Text: "c"},
		{Kind: EventCodeExecutionResult, CodeExecutionResult: result},
		{Kind: EventText, Text: "d"},
		{Kind: EventReasoning, Text: "新", ThoughtSignature: "r2"},
		{Kind: EventReasoning, Text: "段", ThoughtSignature: "r2"},
	} {
		output.observe(event)
	}
	visible := []Part{
		{Text: "你😀", ThoughtSignature: "v"}, {Text: "xy", ThoughtSignature: "v2"},
		{FunctionCall: call}, {Text: "a"}, {InlineData: &Blob{MIME: "image/png", Data: []byte{1}}},
		{Text: "b"}, {ExecutableCode: code}, {Text: "c"}, {CodeExecutionResult: result}, {Text: "d"},
	}
	reasoning := []Part{
		{Text: "思考", Thought: true, ThoughtSignature: "r"},
		{Text: "新段", Thought: true, ThoughtSignature: "r2"},
	}
	if !reflect.DeepEqual(output.visible, visible) || !reflect.DeepEqual(output.reasoning, reasoning) {
		t.Fatalf("parts changed: visible=%+v reasoning=%+v", output.visible, output.reasoning)
	}
	request := GenerateRequest{System: "system", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "question"}}}}}
	usage := localCompleteUsage(request, &output)
	if usage.OutputTokens != localPartsTokens(visible) || usage.ReasoningTokens != localPartsTokens(reasoning) {
		t.Fatalf("merged-Part tokenization changed: %+v", usage)
	}
	counted := countedCompleteUsage(request, &output, TokenCount{InputTokens: 42})
	if counted.InputTokens != 42 || counted.OutputTokens != usage.OutputTokens || counted.ReasoningTokens != usage.ReasoningTokens || counted.TotalTokens != 42+usage.OutputTokens+usage.ReasoningTokens {
		t.Fatalf("counted usage changed: %+v", counted)
	}
}

func TestGeneratedOutputPartsTextSnapshots(t *testing.T) {
	var output generatedOutputParts
	output.observe(Event{Kind: EventText, Text: "prefix"})
	output.observe(Event{Kind: EventReasoning, Text: "thought"})
	visible, reasoning := output.visible[0], output.reasoning[0]
	for i := 0; i < 100; i++ {
		output.observe(Event{Kind: EventText, Text: strings.Repeat("x", 1024)})
		output.observe(Event{Kind: EventReasoning, Text: "more"})
	}
	output.observe(Event{Kind: EventText, Text: "next", ThoughtSignature: "new"})
	output.observe(Event{Kind: EventText, Text: " part", ThoughtSignature: "new"})
	output.observe(Event{Kind: EventReasoning, Text: "next", ThoughtSignature: "new"})
	if visible.Text != "prefix" || reasoning.Text != "thought" {
		t.Fatal("later appends or builder reset mutated an earlier Part value")
	}
	if output.visible[0].Text != "prefix"+strings.Repeat("x", 100*1024) || output.visible[1].Text != "next part" {
		t.Fatal("text was lost across buffer growth or a signature boundary")
	}
}

func TestGeneratedOutputUsageFallback(t *testing.T) {
	for _, kind := range []string{"missing", "partial", "anti_missing", "hide_thinking"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			var wantOutput int64
			fixture.reply = func(request GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
				parts := []Part{{Text: "private ", Thought: true}, {Text: "thought", Thought: true}, {Text: "hello "}, {Text: "world"}}
				wantOutput = localTextTokens("hello world")
				if kind == "anti_missing" {
					parts = []Part{fixtureTransportPart(t, request, "hello world")}
					wantOutput = localPartsTokens(parts)
				}
				body := fixtureResponse(t, rpc, parts, "stop", nil, "")
				if kind == "partial" {
					var root []any
					if err := json.Unmarshal(body, &root); err != nil {
						t.Fatal(err)
					}
					root[0].([]any)[0].([]any)[2] = []any{10, nil, 0}
					body, _ = json.Marshal(root)
				}
				return body, nil
			}
			model := "playground/fixture-model"
			if kind == "anti_missing" {
				model = "playground/anti-truncation/fixture-model"
			}
			events := fixture.generate(t, GenerateRequest{Model: model, Config: GenerationConfig{HideThinking: kind == "hide_thinking"}})
			if pipelineError(events) != nil || pipelineText(events) != "hello world" || pipelineFinish(events) == nil {
				t.Fatalf("output contract changed: %+v", events)
			}
			usage := pipelineUsage(events)
			source := "estimated"
			if kind == "partial" {
				source = "mixed"
			}
			if usage == nil || usage.OutputTokens != wantOutput || usage.Source != source || usage.OutputTokensMissing {
				t.Fatalf("usage fallback changed: %+v, want output %d / %s", usage, wantOutput, source)
			}
			if kind == "missing" || kind == "hide_thinking" {
				if usage.ReasoningTokens != localTextTokens("private thought") {
					t.Fatalf("reasoning merge/hidden usage changed: %+v", usage)
				}
			}
			if kind == "hide_thinking" {
				for _, event := range events {
					if event.Kind == EventReasoning {
						t.Fatal("hidden reasoning leaked")
					}
				}
			}
			if len(fixture.sends) != 1 {
				t.Fatal("usage fallback generated again")
			}
		})
	}
}

type usageCountFixture struct {
	fail bool
}

func (fixture usageCountFixture) Do(_ context.Context, rpc RPCRequest) (*RPCResponse, error) {
	if rpc.Method != "CountTokens" || fixture.fail {
		return nil, errors.New("fixture: count unavailable")
	}
	return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader("[42]"))}, nil
}

func TestGeneratedOutputInteractionStop(t *testing.T) {
	for _, failedCount := range []bool{false, true} {
		t.Run(map[bool]string{false: "counted", true: "count_failed"}[failedCount], func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			fixture.client.transport = usageCountFixture{fail: failedCount}
			fixture.reply = func(_ GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
				if rpc.Method != "CreateInteractionStream" {
					t.Fatalf("unexpected RPC %s", rpc.Method)
				}
				delta := func(text, thought string) []any {
					content := make([]any, 6)
					content[0] = []any{text}
					if thought != "" {
						content[5] = []any{thought}
					}
					event := make([]any, 11)
					event[10] = []any{nil, content}
					return event
				}
				body, err := json.Marshal([]any{[]any{delta("hello", "thinking"), delta(" world STOP after", "")}})
				return body, err
			}
			request := GenerateRequest{
				ID: t.Name(), Model: "fixture-model", AccountID: fixture.account.ID,
				Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "question"}}}},
				Config:   GenerationConfig{StopSequences: []string{" STOP"}, HideThinking: true},
			}
			entry := modelEntry{model: fixture.account.Models[0], defaults: GenerationDefaults{MaxOutputTokens: 32768, DefaultThinkingLevel: 1}}
			source, err := fixture.client.generateInteraction(fixture.ctx, request, entry)
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for event := range source {
				events = append(events, event)
			}
			finish := pipelineFinish(events)
			if pipelineError(events) != nil || pipelineText(events) != "hello world" || finish == nil || finish.FinishReason != "stop_sequence" {
				t.Fatalf("interaction stop changed: %+v", events)
			}
			usage := pipelineUsage(events)
			if failedCount {
				if usage != nil {
					t.Fatalf("count failure invented usage: %+v", usage)
				}
			} else if usage == nil || usage.InputTokens != 42 || usage.OutputTokens != localTextTokens("hello world") || usage.ReasoningTokens != localTextTokens("thinking") || usage.Source != "estimated" {
				t.Fatalf("interaction local usage changed: %+v", usage)
			}
			for _, event := range events {
				if event.Kind == EventReasoning {
					t.Fatal("hidden interaction reasoning leaked")
				}
			}
			if len(fixture.sends) != 1 || !bytes.Contains(fixture.sends[0].Body, []byte("question")) {
				t.Fatal("interaction stop replayed or changed its input")
			}
		})
	}
}

func TestGeneratedOutputGenerateStop(t *testing.T) {
	for _, failedCount := range []bool{false, true} {
		t.Run(map[bool]string{false: "counted", true: "count_failed"}[failedCount], func(t *testing.T) {
			fixture := newPipelineFixture(t, ChannelPlayground)
			fixture.client.transport = usageCountFixture{fail: failedCount}
			fixture.reply = func(_ GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
				parts := []Part{{Text: "thinking", Thought: true}, {Text: "hello"}, {Text: " world STOP after"}}
				return fixtureResponse(t, rpc, parts, "stop", nil, ""), nil
			}
			events := fixture.generate(t, GenerateRequest{
				Model:  "playground/fixture-model",
				Config: GenerationConfig{StopSequences: []string{" STOP"}, HideThinking: true},
			})
			finish := pipelineFinish(events)
			if pipelineError(events) != nil || pipelineText(events) != "hello world" || finish == nil || finish.FinishReason != "stop_sequence" || finish.StopSequence != " STOP" {
				t.Fatalf("plain Generate stop changed: %+v", events)
			}
			usage := pipelineUsage(events)
			if failedCount {
				if usage != nil {
					t.Fatalf("count failure invented usage: %+v", usage)
				}
			} else if usage == nil || usage.InputTokens != 42 || usage.OutputTokens != localTextTokens("hello world") || usage.ReasoningTokens != localTextTokens("thinking") || usage.Source != "estimated" {
				t.Fatalf("plain Generate local usage changed: %+v", usage)
			}
			for _, event := range events {
				if event.Kind == EventReasoning {
					t.Fatal("hidden Generate reasoning leaked")
				}
			}
			if len(fixture.sends) != 1 {
				t.Fatal("local stop generated again")
			}
		})
	}
}
