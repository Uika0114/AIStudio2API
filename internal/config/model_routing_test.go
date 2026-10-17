package config

import "testing"

func TestModelMappingRejectsReservedRoutingNamespaces(t *testing.T) {
	for _, mapping := range []string{
		"build/gemini:gemini", "playground/anti-truncation/gemini:gemini",
		"alias:build/gemini", "alias:models/playground/gemini", "auto/gemini:gemini",
		"anti-truncation/gemini:gemini", "Build/gemini:gemini",
	} {
		if _, err := ParseModelMapping(mapping); err == nil {
			t.Errorf("accepted reserved mapping %q", mapping)
		}
	}
	for _, mapping := range []string{"legacy:gemini", "public:model:models/gemini", "legacy/provider:gemini", "build-compatible/gemini:gemini"} {
		if _, err := ParseModelMapping(mapping); err != nil {
			t.Errorf("legacy mapping %q: %v", mapping, err)
		}
	}
}
