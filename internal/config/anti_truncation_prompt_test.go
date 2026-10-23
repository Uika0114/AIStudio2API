package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntiTruncationSystemPromptConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.env")
	if !Default().AntiTruncationSystemPrompt {
		t.Fatal("default must preserve append")
	}
	if err := os.WriteFile(path, []byte("# legacy config without switch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || !cfg.AntiTruncationSystemPrompt {
		t.Fatalf("legacy default: %v", err)
	}
	for _, enabled := range []bool{false, true} {
		cfg.AntiTruncationSystemPrompt = enabled
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil || loaded.AntiTruncationSystemPrompt != enabled {
			t.Fatalf("round trip %t: %v", enabled, err)
		}
		raw, err := json.Marshal(loaded)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Config
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.AntiTruncationSystemPrompt != enabled {
			t.Fatalf("JSON round trip: %v", err)
		}
	}
	// Environment precedence, strict parsing and legacy JSON compatibility.
	t.Setenv("ANTI_TRUNCATION_SYSTEM_PROMPT", "false")
	loaded, err := Load(path)
	if err != nil || loaded.AntiTruncationSystemPrompt {
		t.Fatalf("env false: %v", err)
	}
	t.Setenv("ANTI_TRUNCATION_SYSTEM_PROMPT", "true")
	loaded, err = Load(path)
	if err != nil || !loaded.AntiTruncationSystemPrompt {
		t.Fatalf("env true: %v", err)
	}
	t.Setenv("ANTI_TRUNCATION_SYSTEM_PROMPT", "not-a-bool")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "ANTI_TRUNCATION_SYSTEM_PROMPT") {
		t.Fatal("invalid bool accepted")
	}
	raw, _ := json.Marshal(Default())
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "anti_truncation_system_prompt")
	raw, _ = json.Marshal(fields)
	var legacy Config
	if err := json.Unmarshal(raw, &legacy); err != nil || !legacy.AntiTruncationSystemPrompt {
		t.Fatalf("legacy JSON default: %v", err)
	}
	fields["anti_truncation_system_prompt"] = json.RawMessage(`"false"`)
	raw, _ = json.Marshal(fields)
	if err := json.Unmarshal(raw, &legacy); err == nil {
		t.Fatal("string bool accepted in JSON")
	}
}
