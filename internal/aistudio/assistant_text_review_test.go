package aistudio

import (
	"errors"
	"strings"
	"testing"
)

// A transcript must not erase a second content variant and thereby bypass the
// unchanged Assistant media/code validator. Such Parts were never valid input.
func TestAssistantTextDowngradeMixedPartCannotHideUnsupportedInput(t *testing.T) {
	for _, result := range []bool{false, true} {
		for _, kind := range []string{"text", "inline", "external", "file", "code", "code_result", "both"} {
			t.Run(kind+map[bool]string{false: "/call", true: "/result"}[result], func(t *testing.T) {
				r := assistantDowngradeRequest()
				p := Part{FunctionCall: &FunctionCall{Name: "fixture"}}
				if result {
					p = Part{FunctionResult: &FunctionResult{Name: "fixture"}}
				}
				switch kind {
				case "text":
					p.Text = "fixture"
				case "inline":
					p.InlineData = &Blob{MIME: "image/png", Data: []byte("fixture")}
				case "external":
					p.ExternalMedia = &ExternalMedia{URL: "https://example.invalid/fixture"}
				case "file":
					p.File = &FileRef{ID: "fixture"}
				case "code":
					p.ExecutableCode = &ExecutableCode{Language: "PYTHON", Code: "fixture"}
				case "code_result":
					p.CodeExecutionResult = &CodeExecutionResult{Outcome: "fixture"}
				case "both":
					p.FunctionCall = &FunctionCall{Name: "fixture"}
					p.FunctionResult = &FunctionResult{Name: "fixture"}
				}
				r.Contents = []Content{{Role: RoleAssistant, Parts: []Part{p}}}
				_, err := prepareAssistantTextRequest(r)
				if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "multiple content variants") {
					t.Fatalf("mixed variant erased: %v", err)
				}
			})
		}
	}
}
