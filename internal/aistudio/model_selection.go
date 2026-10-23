package aistudio

import (
	"fmt"
	"strings"
)

// ModelSelection separates a public model ID from its quota/model identity.
// An empty Channel is the original auto routing policy, never a strict fallback.
type ModelSelection struct {
	RequestedModel string  `json:"requested_model"`
	BaseModel      string  `json:"base_model"`
	Channel        Channel `json:"channel,omitempty"`
	AntiTruncation bool    `json:"anti_truncation,omitempty"`
}

func (selection ModelSelection) Strict() bool { return selection.Channel != "" }

// HasModelSelectionPrefix recognizes the reserved namespace, including malformed
// variants, so unsupported API routes cannot silently interpret it as an alias.
func HasModelSelectionPrefix(modelID string) bool {
	value := strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	head, _, found := strings.Cut(value, "/")
	if !found {
		return false
	}
	switch strings.ToLower(head) {
	case "playground", "build", "assistant", "anti-truncation", "auto":
		return true
	}
	return false
}

// ParseModelSelection parses only the public routing grammar. Alias resolution
// and thinking suffixes belong to the base ID and run after this operation.
func ParseModelSelection(modelID string) (ModelSelection, error) {
	value := strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	result := ModelSelection{RequestedModel: value, BaseModel: value}
	invalid := func() (ModelSelection, error) {
		return ModelSelection{}, fmt.Errorf("%w: invalid routed model ID %q", ErrInvalidArgument, value)
	}
	if value == "" {
		return invalid()
	}
	head, tail, hasSlash := strings.Cut(value, "/")
	if !hasSlash {
		return result, nil
	}
	switch head {
	case string(ChannelPlayground), string(ChannelBuild), string(ChannelAssistant):
		result.Channel = Channel(head)
		if strings.HasPrefix(tail, "anti-truncation/") {
			result.AntiTruncation = true
			tail = strings.TrimPrefix(tail, "anti-truncation/")
		}
		if tail == "" || strings.TrimSpace(tail) != tail || strings.HasPrefix(tail, "/") ||
			strings.HasPrefix(tail, "models/") || HasModelSelectionPrefix(tail) ||
			tail == "anti-truncation" || tail == "auto" {
			return invalid()
		}
		result.BaseModel = tail
	default:
		if HasModelSelectionPrefix(value) {
			return invalid()
		}
	}
	return result, nil
}

// NormalizeGenerateRequestModel is idempotent. Once parsed, canonicalizing an
// alias must not erase the requested policy. A mismatched internal policy fails
// closed instead of silently turning a strict request into auto.
func NormalizeGenerateRequestModel(request GenerateRequest) (GenerateRequest, error) {
	if request.Selection.RequestedModel == "" {
		selection, err := ParseModelSelection(request.Model)
		if err != nil {
			return request, err
		}
		request.Selection = selection
		request.Model = selection.BaseModel
		return request, nil
	}
	expected, err := ParseModelSelection(request.Selection.RequestedModel)
	if err != nil {
		return request, err
	}
	if expected.Channel != request.Selection.Channel || expected.AntiTruncation != request.Selection.AntiTruncation {
		return request, fmt.Errorf("%w: model routing policy was changed after parsing", ErrInvalidArgument)
	}
	current, err := ParseModelSelection(request.Model)
	if err != nil {
		return request, err
	}
	if current.Strict() {
		if current.Channel != expected.Channel || current.AntiTruncation != expected.AntiTruncation {
			return request, fmt.Errorf("%w: conflicting model routing policies", ErrInvalidArgument)
		}
		request.Model = current.BaseModel
	} else {
		request.Model = current.BaseModel
	}
	request.Selection.BaseModel = request.Model
	return request, nil
}

// PublicModelID returns an ID suitable for protocol model fields and catalogs.
// The caller supplies the canonical base or an existing public alias.
func (selection ModelSelection) PublicModelID(base string) string {
	if !selection.Strict() {
		return base
	}
	prefix := string(selection.Channel) + "/"
	if selection.AntiTruncation {
		prefix += "anti-truncation/"
	}
	return prefix + base
}

// ValidateChannel rejects inconsistent leases before model lookup or generation.
func (selection ModelSelection) ValidateChannel(actual Channel) error {
	if selection.Strict() && selection.Channel != actual {
		return fmt.Errorf("%w: requested channel %s cannot use %s", ErrInvalidArgument, selection.Channel, actual)
	}
	return nil
}
