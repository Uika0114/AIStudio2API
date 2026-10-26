package aistudio

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Tool controls are intentionally downgraded only on the Assistant text route.
func TestAssistantToolBoundaryParallelBecomesText(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, assistantPart("fixture answer", false))))
			request := assistantCoreRequest(lease)
			request.Tools.ToolConfig.ParallelCalls = &parallel
			source, err := fixture.client.Generate(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for e := range source {
				events = append(events, e)
			}
			if pipelineError(events) != nil || pipelineText(events) != "fixture answer" || pipelineFinish(events) == nil {
				t.Fatalf("text downgrade: %+v", events)
			}
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			_, protected, attempts := fixture.snapshot()
			if len(protected) != 1 || len(attempts) != 1 {
				t.Fatal("wrong generation count")
			}
			if request.Tools.ToolConfig.ParallelCalls == nil || *request.Tools.ToolConfig.ParallelCalls != parallel {
				t.Fatal("mutated caller")
			}
		})
	}
}

func TestAssistantToolBoundaryDirectEncoderDowngradesToolsOnly(t *testing.T) {
	cases := map[string]func(*GenerateRequest){
		"declaration": func(r *GenerateRequest) {
			r.Tools.Functions = []FunctionDeclaration{{Name: "read_fixture", Parameters: json.RawMessage(`{"type":"object"}`)}}
		},
		"required":       func(r *GenerateRequest) { r.Tools.ToolConfig.Mode = "required" },
		"parallel_false": func(r *GenerateRequest) { v := false; r.Tools.ToolConfig.ParallelCalls = &v },
		"parallel_true":  func(r *GenerateRequest) { v := true; r.Tools.ToolConfig.ParallelCalls = &v },
		"history": func(r *GenerateRequest) {
			r.Contents = append(r.Contents, Content{Role: RoleAssistant, Parts: []Part{{FunctionCall: &FunctionCall{ID: "fixture-call", Name: "read_fixture", Arguments: json.RawMessage(`{}`)}}}})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := assistantToolTestRequest()
			mutate(&request)
			body, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request")
			if err != nil {
				t.Fatal(err)
			}
			want, err := prepareAssistantTextRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := encodeContents(want.Contents)
			expected, _ := json.Marshal(encoded)
			var wire []json.RawMessage
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			if string(wire[0]) != string(expected) {
				t.Fatalf("history mismatch: %s != %s", wire[0], expected)
			}
			for i, raw := range wire {
				if !isJSONNull(raw) && i != 0 && i != 5 && i != 7 && i != 8 && i != 11 && i != 13 && i != 15 && i != 18 && i != 28 {
					t.Fatalf("unexpected wire field: %d", i)
				}
			}
		})
	}
	for _, kind := range []string{"anti", "nonpositive_token_budget", "code_history", "unknown_google"} {
		t.Run(kind, func(t *testing.T) {
			request := assistantToolTestRequest()
			switch kind {
			case "anti":
				request.Selection.AntiTruncation = true
			case "nonpositive_token_budget":
				v := int64(0)
				request.Config.MaxOutputTokens = &v
			case "code_history":
				request.Contents[0].Parts = []Part{{ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "fixture"}}}
			case "unknown_google":
				request.Tools.Google = []string{"not-a-known-tool"}
			}
			body, err := EncodeAssistantRequest(request, GenerationDefaults{}, "fixture-applet", "fixture-request")
			if !errors.Is(err, ErrInvalidArgument) || body != nil {
				t.Fatalf("non-tool contract dropped: %s %v", body, err)
			}
		})
	}
}

// Even with request downgrade, unimplemented native output must not become text success.
func TestAssistantToolBoundaryUnmappedResponseFailsWithoutReplay(t *testing.T) {
	for _, slot := range []int{assistantFunctionCallPartIndex, assistantFunctionResponsePartIndex} {
		t.Run(fmt.Sprint(slot), func(t *testing.T) {
			fixture, ctx, lease := assistantCoreFixture(t)
			part := make([]any, 15)
			part[1] = "must not leak from an unmapped Part"
			part[slot] = []any{"read_fixture", []any{}, "fixture-call"}
			fixture.streamBody = assistantStreamBytes(assistantMessage(assistantStep("model", 1, 1, []any{"10", "5", "2"}, part)))
			source, err := fixture.client.Generate(ctx, assistantCoreRequest(lease))
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			for e := range source {
				events = append(events, e)
			}
			err = pipelineError(events)
			var evidence *ProtocolEvidenceError
			if !errors.As(err, &evidence) || !strings.Contains(evidence.Detail, "shared") || !strings.Contains(evidence.Detail, "output mapping not implemented") {
				t.Fatalf("evidence lost: %v", err)
			}
			if pipelineText(events) != "" || pipelineFinish(events) != nil {
				t.Fatalf("unsupported output became success: %+v", events)
			}
			if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 17 || usage.Source != "upstream" {
				t.Fatalf("usage lost: %+v", usage)
			}
			fixture.waitCleanup(t, assistantCancelMethod)
			fixture.waitCleanup(t, assistantDeleteAppletMethod)
			_, protected, attempts := fixture.snapshot()
			if len(protected) != 1 || len(attempts) != 1 {
				t.Fatal("response caused replay")
			}
		})
	}
}
