package chromeauth

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// These three source fields match the frozen 20261022-anti-reply-schema reader.
// The test intentionally proves data loss, not safe rollback to that reader.
type frozenChromeSource struct {
	Browser string `json:"browser"`
	Profile string `json:"profile,omitempty"`
	Email   string `json:"email,omitempty"`
}
type frozenChromeExtension struct {
	Source frozenChromeSource            `json:"source"`
	OAuth  *aistudio.ChromeOAuthMaterial `json:"oauth,omitempty"`
}

func TestFrozenLegacySourceRewriteLosesRootAndRequiresRebinding(t *testing.T) {
	state := aistudio.StorageState{}
	ext := aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", ChromeRoot: "C:/FixtureDedicated", Profile: "Default", GaiaID: "111", Email: "fixture@example.com"}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: "111", RefreshToken: "fixture-refresh-token", WrappedBindingKey: []byte("fixture-binding")}}
	if err := state.SetAuthExtension(ext); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var retained aistudio.StorageState
	if err := json.Unmarshal(raw, &retained); err != nil {
		t.Fatal(err)
	}
	kept, _, _ := retained.AuthExtension()
	if !SameSource(kept.Source, ext.Source) {
		t.Fatal("raw state round trip lost the source")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var legacy frozenChromeExtension
	if err := json.Unmarshal(fields["aistudio2api"], &legacy); err != nil {
		t.Fatal(err)
	}
	fields["aistudio2api"], err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var rewritten aistudio.StorageState
	if err := json.Unmarshal(raw, &rewritten); err != nil {
		t.Fatal(err)
	}
	lost, exists, err := rewritten.AuthExtension()
	if err != nil || !exists || lost.Source.ChromeRoot != "" || lost.Source.GaiaID != "" || lost.OAuth == nil || lost.OAuth.GaiaID != "111" || lost.OAuth.RefreshToken != ext.OAuth.RefreshToken {
		t.Fatal("legacy fixture does not model the frozen typed rewrite")
	}
	if _, err := ValidateSource(lost.Source, lost.OAuth); !errors.Is(err, ErrSourceBindingRequired) {
		t.Fatal("source loss was silently repaired with a guessed default root")
	}
}
