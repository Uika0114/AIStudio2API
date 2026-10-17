package aistudio

import (
	"errors"
	"testing"
)

func TestParseModelSelection(t *testing.T) {
	tests := []struct {
		id, base string
		channel  Channel
		anti     bool
	}{
		{"gemini-4-argon", "gemini-4-argon", "", false},
		{"playground/gemini-4-argon", "gemini-4-argon", ChannelPlayground, false},
		{"models/build/gemini-3.8-flash", "gemini-3.8-flash", ChannelBuild, false},
		{"playground/anti-truncation/alias-high", "alias-high", ChannelPlayground, true},
		{"build/anti-truncation/alias", "alias", ChannelBuild, true},
		{"legacy/name", "legacy/name", "", false},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			selection, err := ParseModelSelection(test.id)
			if err != nil {
				t.Fatal(err)
			}
			if selection.BaseModel != test.base || selection.Channel != test.channel || selection.AntiTruncation != test.anti {
				t.Fatalf("unexpected selection: %+v", selection)
			}
			if selection.Strict() != (test.channel != "") {
				t.Fatal("strict policy mismatch")
			}
			again, err := ParseModelSelection(selection.PublicModelID(test.base))
			if err != nil || again.BaseModel != selection.BaseModel || again.Channel != selection.Channel || again.AntiTruncation != selection.AntiTruncation {
				t.Fatalf("round trip: %+v %v", again, err)
			}
		})
	}
}

func TestParseModelSelectionRejectsMalformedPolicy(t *testing.T) {
	for _, id := range []string{"", "build/", "playground/anti-truncation", "build/anti-truncation/", "auto/x", "anti-truncation/x", "Build/x", "playground/build/x", "build/anti-truncation/playground/x", "build/models/x", "build//x", "build/ x"} {
		if _, err := ParseModelSelection(id); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q: %v", id, err)
		}
	}
}

func TestNormalizeGenerateRequestModelPreservesPolicyAcrossAliases(t *testing.T) {
	request, err := NormalizeGenerateRequestModel(GenerateRequest{Model: "build/anti-truncation/public-alias"})
	if err != nil {
		t.Fatal(err)
	}
	request.Model = "gemini-4-argon"
	request, err = NormalizeGenerateRequestModel(request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Selection.RequestedModel != "build/anti-truncation/public-alias" || request.Selection.BaseModel != request.Model || request.Selection.Channel != ChannelBuild || !request.Selection.AntiTruncation {
		t.Fatalf("lost policy: %+v", request.Selection)
	}
	if err := request.Selection.ValidateChannel(ChannelPlayground); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("wrong lease allowed: %v", err)
	}
	if err := request.Selection.ValidateChannel(ChannelBuild); err != nil {
		t.Fatal(err)
	}
	request.Selection.Channel = ""
	if _, err := NormalizeGenerateRequestModel(request); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cleared policy allowed: %v", err)
	}
}

func TestNormalizeGenerateRequestModelDoesNotMutateOriginal(t *testing.T) {
	original := GenerateRequest{Model: "playground/gemini-4-argon"}
	normalized, err := NormalizeGenerateRequestModel(original)
	if err != nil {
		t.Fatal(err)
	}
	if original.Selection.RequestedModel != "" || original.Model == normalized.Model {
		t.Fatal("input mutated")
	}
	again, err := NormalizeGenerateRequestModel(normalized)
	if err != nil || again.Selection != normalized.Selection || again.Model != normalized.Model {
		t.Fatalf("not idempotent: %+v %v", again, err)
	}
}
