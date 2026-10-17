package aistudio

import "fmt"

// validateStrictGenerateEntry uses the FINAL channel catalog, and refuses the
// legacy tool-dropping/transcription and output-budget clamping behavior.
func validateStrictGenerateEntry(request GenerateRequest, entry modelEntry) error {
	if !request.Selection.Strict() {
		return nil
	}
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, message) }
	activeTools := request.Tools.ToolConfig.Mode != "none"
	if !entry.model.Capabilities["function_declarations"] {
		if request.Selection.AntiTruncation || activeTools && len(request.Tools.Functions) > 0 {
			return invalid("selected channel/model does not support requested function tools")
		}
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part.FunctionCall != nil || part.FunctionResult != nil {
					return invalid("selected channel/model cannot preserve native function history")
				}
			}
		}
	}
	if activeTools {
		for _, name := range request.Tools.Google {
			capability := name
			if name == "url_context" {
				capability = "browse"
			}
			if !entry.model.Capabilities[capability] {
				return invalid("selected channel/model does not support Google tool " + name)
			}
		}
		if search := request.Tools.GoogleSearch; search != nil {
			if (search.WebSearch || !search.ImageSearch) && !entry.model.Capabilities["google_search"] || search.ImageSearch && !entry.model.Capabilities["image_search"] {
				return invalid("selected channel/model does not support requested search capabilities")
			}
		}
	}
	if requested := request.Config.MaxOutputTokens; requested != nil {
		limit := max(entry.defaults.MaxOutputTokens, entry.defaults.ContinuedOutputTokens)
		if *requested <= 0 || limit <= 0 || *requested > limit {
			return invalid("requested output budget cannot be honored by the selected channel/model")
		}
	}
	if request.Selection.AntiTruncation {
		for _, modality := range request.Config.ResponseModalities {
			if modality != ResponseModalityText {
				return invalid("anti-truncation currently transports text only")
			}
		}
		if len(request.Config.ResponseSchema) > 0 || request.Config.ResponseMIMEType != "" && request.Config.ResponseMIMEType != "text/plain" {
			return invalid("anti-truncation cannot preserve an enforced response schema or non-text MIME contract")
		}
	}
	return nil
}
