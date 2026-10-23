package aistudio

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Check the model-visible declaration independently of the optional system prompt.
func assertReplySubmissionSchema(t *testing.T, declaration FunctionDeclaration) {
	t.Helper()
	for _, rule := range []string{
		"Submit the complete user-facing reply for the current interaction.",
		"Every user-facing reply MUST be submitted through this function",
		"answers, clarification questions, confirmations, refusals, and progress updates",
		"not completion of the overall task",
		"displayed verbatim to the user",
		"exactly one call for that reply and at most one call per model response",
		"cannot delete, replace, retract, or revise any previously emitted text",
		"performs no external action",
		"a response containing only real tool calls needs no reply submission",
		"cannot satisfy a required real tool call",
	} {
		if !strings.Contains(declaration.Description, rule) {
			t.Fatalf("tool description missing reply contract: %q", rule)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(declaration.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The complete reply to display to the user, exactly as written. Include only the reply itself, not drafts, alternative versions, self-editing commentary, or instructions to change the submitted text. Such instructions would be displayed as ordinary text, not executed.",
			},
		},
		"required": []any{"content"},
	}
	if !reflect.DeepEqual(schema, want) {
		t.Fatalf("unexpected parameter schema: %s", declaration.Parameters)
	}
}

func TestAntiTruncationReplySchemaIndependentOfSystemPrompt(t *testing.T) {
	for _, appendPrompt := range []bool{false, true} {
		for _, collision := range []bool{false, true} {
			t.Run(fmt.Sprintf("append=%t/collision=%t", appendPrompt, collision), func(t *testing.T) {
				request := GenerateRequest{System: "caller system", Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}}
				wantName := antiTruncationTransportBaseName
				if collision {
					request.Tools.Functions = []FunctionDeclaration{{Name: wantName}}
					wantName += "_2"
				}
				upstream, transport, err := prepareAntiTruncation(request, appendPrompt)
				if err != nil {
					t.Fatal(err)
				}
				if transport.Name() != wantName {
					t.Fatal("tool identity/collision behavior changed")
				}
				if !appendPrompt && upstream.System != request.System {
					t.Fatal("schema update injected a disabled system instruction")
				}
				declaration := upstream.Tools.Functions[len(upstream.Tools.Functions)-1]
				assertReplySubmissionSchema(t, declaration)
				if declaration.Name != transport.Name() {
					t.Fatal("schema and projector name differ")
				}
			})
		}
	}
}
