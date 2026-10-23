package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestAssistantDispatchQueueAndTimeoutAreIndependent(t *testing.T) {
	seen := map[string]bool{}
	for _, channel := range []aistudio.Channel{"", aistudio.ChannelPlayground, aistudio.ChannelBuild, aistudio.ChannelAssistant} {
		selection := aistudio.AccountSelection{ModelID: "gemini-4-argon", Method: "generateContent", Channel: channel, StrictChannel: channel != ""}
		key := dispatchKey(selection)
		if seen[key] {
			t.Fatal("assistant shared a strict/auto queue")
		}
		seen[key] = true
	}
	model := aistudio.Model{ID: "gemini-4-argon", Methods: []string{"generateContent"}, InputTokenLimit: 1048576, OutputTokenLimit: 262144, Capabilities: map[string]bool{"output_continuation": true}}
	pool := aistudio.NewAccountPool([]*aistudio.Account{{ID: "fixture", Config: aistudio.AccountConfig{Enabled: true}, State: aistudio.AccountReady, Models: []aistudio.Model{model}}}, 1)
	service := &trackedService{pool: pool, timeout: time.Minute}
	request, _ := aistudio.NormalizeGenerateRequestModel(aistudio.GenerateRequest{Model: "assistant/gemini-4-argon"})
	if got := service.generateTimeout(request); got != time.Minute {
		t.Fatalf("borrowed continuation timeout: %v", got)
	}
	request, _ = aistudio.NormalizeGenerateRequestModel(aistudio.GenerateRequest{Model: "gemini-4-argon"})
	if got := service.generateTimeout(request); got <= time.Minute {
		t.Fatal("changed legacy auto continuation")
	}
}

func TestAssistantLateQuotaRetainsChannelAndAdminIdentity(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "global"}[global], func(t *testing.T) {
			model := aistudio.Model{ID: "gemini-4-argon", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "capability_code_48": true}}
			account := &aistudio.Account{ID: "fixture-assistant-quota", Config: aistudio.AccountConfig{Enabled: true, Label: "fixture"}, State: aistudio.AccountReady, Models: []aistudio.Model{model}}
			pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
			pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild, aistudio.ChannelAssistant})
			if err := pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
				t.Fatal(err)
			}
			selection := aistudio.AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: aistudio.ChannelAssistant, StrictChannel: true}
			lease, _, err := pool.TryAcquireFor(context.Background(), selection)
			if err != nil || lease == nil {
				t.Fatalf("lease: %v", err)
			}
			metadata := map[string]string{}
			if global {
				metadata["quota_metric"] = "fixture_global"
			}
			upstreamError := &aistudio.RPCError{Method: "StreamCodeAssistantOfflineGeneration", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "fixture quota", RetryDelay: time.Hour, Metadata: metadata}
			source := make(chan aistudio.Event, 1)
			source <- aistudio.Event{Kind: aistudio.EventError, Err: upstreamError}
			close(source)
			destination := make(chan aistudio.Event, 4)
			requestCtx, cancel := context.WithCancel(context.Background())
			service := &trackedService{pool: pool, requests: &requestRegistry{active: map[string]trackedRequest{}}}
			service.forwardEvents(context.Background(), requestCtx, cancel, "fixture-assistant-request", aistudio.Event{Kind: aistudio.EventText, Text: "already delivered", ProviderModel: model.ID}, source, destination, lease, nil, &upstreamActivity{}, model.ID)
			var errorSeen, finished bool
			for event := range destination {
				errorSeen = errorSeen || event.Kind == aistudio.EventError
				finished = finished || event.Kind == aistudio.EventFinish
			}
			if !errorSeen || finished {
				t.Fatal("assistant late quota became success")
			}
			if other, _, _ := pool.TryAcquireFor(context.Background(), selection); other != nil {
				_ = other.Release()
				t.Fatal("assistant escaped own quota")
			}
			selection.Channel = aistudio.ChannelBuild
			other, _, err := pool.TryAcquireFor(context.Background(), selection)
			if global {
				if other != nil {
					_ = other.Release()
					t.Fatal("global quota did not constrain Build")
				}
			} else {
				if err != nil || other == nil {
					t.Fatalf("assistant local quota constrained Build: %v", err)
				}
				if err := other.Release(); err != nil {
					t.Fatal(err)
				}
				cooldowns, err := (&runtimeAdmin{pool: pool}).Cooldowns(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(cooldowns) != 1 || cooldowns[0].Channel != "assistant" || cooldowns[0].ModelID != model.ID {
					t.Fatalf("mislabelled cooldown: %+v", cooldowns)
				}
			}
		})
	}
}
