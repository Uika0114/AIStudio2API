package aistudio

import (
	"context"
	"fmt"
	"strings"
)

// assistantSupportsModelLocked is eligibility, not proof of quota or upstream
// acceptance. It deliberately does not consult the Build proxy catalog/cooldown.
func (p *AccountPool) assistantSupportsModelLocked(account *Account, modelID string, anyTier bool) bool {
	if account == nil {
		return false
	}
	for _, model := range account.Models {
		if !modelMatchesID(model, modelID) || !hasMethod(model, "generateContent") {
			continue
		}
		if !assistantCapableModel(model) {
			continue
		}
		return anyTier || modelAllowedByTier(model, account.BenefitTier)
	}
	return false
}

// AssistantModelVariant describes what THIS adapter implements, not a claim
// about all capabilities of the underlying model or Google's private protocol.
// Unknown native token bounds stay zero, never borrowed from PG continuation.
func AssistantModelVariant(model Model) Model {
	model = cloneAccountModels([]Model{model})[0]
	const budgetNotice = "Assistant ignores explicit output token budgets; server-side generation settings apply."
	if !strings.Contains(model.Description, budgetNotice) {
		model.Description = strings.TrimSpace(model.Description + " " + budgetNotice)
	}
	model.Methods = []string{"generateContent"}
	model.Channels = []string{string(ChannelAssistant)}
	model.InputTokenLimit, model.OutputTokenLimit = 0, 0
	capabilities := map[string]bool{
		"chat_model": true, "assistant_route": true,
		"function_declarations": assistantFunctionToolsImplemented(),
	}
	for _, name := range []string{"thinking", "thinking_level"} {
		if model.Capabilities[name] {
			capabilities[name] = true
		}
	}
	model.Capabilities = capabilities
	model.CapabilityOptions = map[string][]string{}
	return model
}

func (c *Client) assistantModelEntry(ctx context.Context, request GenerateRequest, lease *AccountLease) (modelEntry, error) {
	if lease == nil || lease.Channel() != ChannelAssistant {
		return modelEntry{}, fmt.Errorf("%w: assistant requires its own channel lease", ErrInvalidArgument)
	}
	entry, err := c.modelEntry(ctx, request.AccountID, request.Model)
	if err != nil {
		return modelEntry{}, err
	}
	if !assistantCapableModel(entry.model) {
		return modelEntry{}, fmt.Errorf("%w: no known assistant model mapping for %s", ErrModelNotFound, strings.TrimSpace(request.Model))
	}
	entry.model = AssistantModelVariant(entry.model)
	// Generation budgets/continuation from PG must not leak into Assistant.
	entry.defaults = GenerationDefaults{
		Thinking: true, ThinkingLevel: true, DefaultThinkingLevel: assistantDefaultThinkingLevel,
		ThinkingLevels: append([]int64(nil), entry.defaults.ThinkingLevels...),
	}
	return entry, nil
}
