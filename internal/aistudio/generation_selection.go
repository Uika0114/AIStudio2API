package aistudio

import (
	"fmt"
	"strings"
)

// GenerationAccountSelection keeps explicit policy separate from legacy hints.
// Build uses its own native output limit; a PG continuation estimate alone must
// not invent a Build token limit. The final Build model entry validates budget.
func GenerationAccountSelection(request GenerateRequest, resourceID string, continuationRounds int) (AccountSelection, error) {
	selection := AccountSelection{
		ModelID: strings.TrimPrefix(strings.TrimSpace(request.Model), "models/"),
		Method:  "generateContent", AccountID: strings.TrimSpace(request.AccountID), ResourceID: resourceID,
		Channel: request.Selection.Channel, StrictChannel: request.Selection.Strict(),
	}
	selection.PlaygroundOnly = resourceID != "" || request.Config.SpeechConfig != nil && request.Config.SpeechConfig.Mode != ""
	if continuationRounds > 1 && !(selection.StrictChannel && selection.Channel != ChannelPlayground) {
		selection.PlaygroundOnly = true
	}
	if selection.StrictChannel {
		if selection.Channel != ChannelPlayground && selection.PlaygroundOnly {
			return selection, fmt.Errorf("%w: request requires Playground-only resources or speech mode, but %s was selected", ErrInvalidArgument, selection.Channel)
		}
		if selection.Channel == ChannelPlayground && RequestNeedsBuildSchema(request) {
			return selection, fmt.Errorf("%w: request schema requires Build; Playground was explicitly selected", ErrInvalidArgument)
		}
	}
	return selection, nil
}

// Only explicit routes get new cursor namespaces. Auto retains its prior key.
func selectionRoutingKey(selection AccountSelection) string {
	key := selectionAccessScope(selection)
	if selection.StrictChannel {
		return string(selection.Channel) + "\x00" + key
	}
	return key
}
