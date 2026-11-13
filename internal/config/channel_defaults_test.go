package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUpstreamChannelDefaultsAndExplicitOverrides(t *testing.T) {
	// Isolate all sources: no user's environment or configuration is consulted.
	for _, key := range configKeys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "fixture.env")
	cfg, err := Load(path)
	if err != nil || !reflect.DeepEqual(cfg.UpstreamChannels, []string{"playground"}) {
		t.Fatalf("unconfigured defaults: %v %v", cfg.UpstreamChannels, err)
	}
	for _, channels := range [][]string{{"playground", "build"}, {"playground", "build", "assistant"}, {"build"}, {"assistant"}, {"playground"}} {
		cfg.UpstreamChannels = channels
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		saved, err := Load(path)
		if err != nil || !reflect.DeepEqual(saved.UpstreamChannels, channels) {
			t.Fatalf("explicit saved channels changed: %v %v", saved.UpstreamChannels, err)
		}
	}
	t.Setenv("UPSTREAM_CHANNELS", "build,assistant")
	cfg, err = Load(path)
	if err != nil || !reflect.DeepEqual(cfg.UpstreamChannels, []string{"build", "assistant"}) {
		t.Fatalf("environment did not override saved Playground: %v %v", cfg.UpstreamChannels, err)
	}
	t.Setenv("UPSTREAM_CHANNELS", "")
	if _, err := Load(path); err == nil {
		t.Fatal("empty explicit environment silently restored defaults")
	}
}
