package api

import (
	"context"
	"fmt"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// generateWithHistory is the shared generation boundary for Chat, Responses
// (including background/compaction) and Gemini. It never changes route policy.
func (s *server) generateWithHistory(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	var err error
	request, err = normalizeRoutedModel(request)
	if err != nil {
		return nil, err
	}
	request.Contents = cloneSignatureContents(request.Contents)
	if request.Selection.Strict() && contentsNeedSignatureScope(request.Contents) {
		models, lookupErr := s.service.Models(ctx)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if model, ok := lookupCatalogBaseModel(models, request.Selection.BaseModel); ok && model.BaseModel != "" {
			request.Selection.BaseModel = model.BaseModel
		}
	}
	if err := s.thoughtSignatures.RestoreChecked(request.Contents, request.Selection); err != nil {
		return nil, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	source, err := s.service.Generate(childCtx, request)
	if err != nil {
		cancel()
		return nil, err
	}
	events := make(chan aistudio.Event, 1)
	go func() {
		defer close(events)
		defer cancel()
		for event := range source {
			if request.Selection.Strict() && event.Channel != "" && event.Channel != request.Selection.Channel {
				event = aistudio.Event{Kind: aistudio.EventError, Err: fmt.Errorf("%w: actual generation channel violates explicit model policy", aistudio.ErrInvalidArgument)}
				select {
				case events <- event:
				case <-childCtx.Done():
				}
				return
			}
			s.thoughtSignatures.RememberEvent(event, request.Selection)
			select {
			case events <- event:
			case <-childCtx.Done():
				return
			}
		}
	}()
	return events, nil
}

func contentsNeedSignatureScope(contents []aistudio.Content) bool {
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil || part.ThoughtSignature != "" {
				return true
			}
		}
	}
	return false
}

// Only function calls are modified by signature restoration. Detach them before
// concurrent choices/history reuse; never mutate the saved response or a sibling.
func cloneSignatureContents(contents []aistudio.Content) []aistudio.Content {
	cloned := cloneResponseContents(contents)
	for i := range cloned {
		for j := range cloned[i].Parts {
			part := &cloned[i].Parts[j]
			if part.FunctionCall != nil {
				call := *part.FunctionCall
				call.Arguments = append([]byte(nil), call.Arguments...)
				part.FunctionCall = &call
			}
		}
	}
	return cloned
}
