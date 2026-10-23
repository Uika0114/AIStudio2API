package config

import (
	"reflect"
	"testing"
)

func TestAssistantChannelIsOptInAndReserved(t *testing.T) {
	if got := Default().UpstreamChannels; !reflect.DeepEqual(got, []string{"playground", "build"}) {
		t.Fatalf("default enabled new upstream path: %v", got)
	}
	for _, channels := range [][]string{{"assistant"}, {"playground", "build", "assistant"}} {
		if err := validateUpstreamChannels(channels); err != nil {
			t.Fatalf("%v: %v", channels, err)
		}
	}
	for _, channels := range [][]string{nil, {"unknown"}, {"assistant", "assistant"}} {
		if err := validateUpstreamChannels(channels); err == nil {
			t.Fatalf("invalid channels accepted: %v", channels)
		}
	}
	for _, mapping := range []string{"assistant/m:gemini-4-argon", "alias:assistant/gemini-4-argon", "alias:Assistant/gemini-4-argon"} {
		if _, err := ParseModelMapping(mapping); err == nil {
			t.Fatalf("mapping bypassed route: %s", mapping)
		}
	}
}
