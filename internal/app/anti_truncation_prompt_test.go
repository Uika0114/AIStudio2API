package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func TestAntiTruncationSystemPromptAdminPersistenceAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.env")
	active := config.Default()
	if err := active.Save(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx), pool: aistudio.NewAccountPool(nil, 1)}
	manager := &runtimeManager{activeManagement: active}
	for _, enabled := range []bool{false, true} {
		value, err := admin.RuntimeConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if value.AntiTruncationSystemPrompt == nil {
			t.Fatal("GET omitted current value")
		}
		value.AntiTruncationSystemPrompt = &enabled
		updated, err := admin.UpdateRuntimeConfig(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		if updated.AntiTruncationSystemPrompt == nil || *updated.AntiTruncationSystemPrompt != enabled {
			t.Fatal("save response lost value")
		}
		saved, err := config.Load(path)
		if err != nil || saved.AntiTruncationSystemPrompt != enabled {
			t.Fatalf("persistence: %v", err)
		}
		decorated := manager.decorateRuntimeConfig(updated, active)
		if decorated.ManagementRestartRequired || decorated.ServiceRestartRequired != !enabled {
			t.Fatal("wrong restart boundary")
		}
		// Older management clients omit the new field; saving other settings must not reset it.
		raw, _ := json.Marshal(updated)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "anti_truncation_system_prompt")
		raw, _ = json.Marshal(fields)
		var oldClient api.RuntimeConfig
		if err := json.Unmarshal(raw, &oldClient); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.UpdateRuntimeConfig(ctx, oldClient); err != nil {
			t.Fatal(err)
		}
		saved, err = config.Load(path)
		if err != nil || saved.AntiTruncationSystemPrompt != enabled {
			t.Fatalf("old client reset flag: %v", err)
		}
		if !sameDataConfig(runtimeConfigDTO(saved), saved, dataConfigOverrides{}) {
			t.Fatal("restarted config still reports stale")
		}
	}
}
