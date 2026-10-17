package api

import (
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"strings"
	"testing"
)

func TestStrictCatalogVariantsPreserveCanonicalBaseAndNativeLimits(t *testing.T) {
	model := routingTestModel()
	model.ID = "friendly"
	model.BaseModel = "gemini-4-argon"
	model.ChannelTokenLimits = map[aistudio.Channel]aistudio.ModelTokenLimits{aistudio.ChannelPlayground: {Input: 1048576, Output: 262144}, aistudio.ChannelBuild: {Input: 131072, Output: 8192}}
	for _, id := range []string{"build/friendly", "build/anti-truncation/friendly"} {
		got, ok := LookupPublicModel([]aistudio.Model{model}, id)
		if !ok || got.ID != id || got.BaseModel != "gemini-4-argon" || got.InputTokenLimit != 131072 || got.OutputTokenLimit != 8192 || got.Capabilities["output_continuation"] {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	if !model.Capabilities["output_continuation"] {
		t.Fatal("Build projection mutated PG metadata")
	}
	embed := aistudio.Model{ID: "embed", Methods: []string{"embedContent"}, Channels: []string{"build"}}
	for _, listed := range publicModelCatalog([]aistudio.Model{embed}) {
		if strings.HasPrefix(listed.ID, "build/") {
			t.Fatal("advertised a reserved prefix on unsupported endpoint")
		}
	}
	if _, ok := LookupPublicModel([]aistudio.Model{embed}, "build/embed"); ok {
		t.Fatal("lookup admits unsupported variant")
	}
}
