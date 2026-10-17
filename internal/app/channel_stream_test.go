package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestLateGenerationQuotaWritesBackActualChannel(t *testing.T) {
	for _, global := range []bool{false, true} {
		name := "channel"
		if global {
			name = "account-global"
		}
		t.Run(name, func(t *testing.T) {
			model := aistudio.Model{ID: "fixture-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
			account := &aistudio.Account{ID: "fixture-late-quota", Config: aistudio.AccountConfig{Enabled: true, Label: "fixture"}, State: aistudio.AccountReady, Models: []aistudio.Model{model}}
			pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
			pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
			if err := pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
				t.Fatal(err)
			}
			selection := aistudio.AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: aistudio.ChannelBuild, StrictChannel: true}
			lease, _, err := pool.TryAcquireFor(context.Background(), selection)
			if err != nil || lease == nil {
				t.Fatalf("lease: %v", err)
			}
			metadata := map[string]string{}
			if global {
				metadata["quota_metric"] = "fixture_global"
			}
			upstreamError := &aistudio.RPCError{Method: "ProxyStreamedCall", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "fixture quota", RetryDelay: time.Hour, Metadata: metadata}
			source := make(chan aistudio.Event, 1)
			source <- aistudio.Event{Kind: aistudio.EventError, Err: upstreamError}
			close(source)
			destination := make(chan aistudio.Event, 4)
			requestCtx, cancel := context.WithCancel(context.Background())
			service := &trackedService{pool: pool, requests: &requestRegistry{active: map[string]trackedRequest{}}}
			service.forwardEvents(context.Background(), requestCtx, cancel, "fixture-request", aistudio.Event{Kind: aistudio.EventText, Text: "already delivered", ProviderModel: model.ID}, source, destination, lease, nil, &upstreamActivity{}, model.ID)
			var text, errorSeen, finished bool
			for event := range destination {
				text = text || event.Kind == aistudio.EventText
				errorSeen = errorSeen || event.Kind == aistudio.EventError
				finished = finished || event.Kind == aistudio.EventFinish
			}
			if !text || !errorSeen || finished {
				t.Fatal("late quota laundered into success")
			}
			build, _, _ := pool.TryAcquireFor(context.Background(), selection)
			if build != nil {
				_ = build.Release()
				t.Fatal("Build cooldown missing after first semantic event")
			}
			selection.Channel = aistudio.ChannelPlayground
			pg, _, pgErr := pool.TryAcquireFor(context.Background(), selection)
			if global {
				if pg != nil {
					_ = pg.Release()
					t.Fatal("global quota did not constrain PG")
				}
			} else {
				if pgErr != nil || pg == nil {
					t.Fatalf("Build quota damaged PG: %v", pgErr)
				}
				if err := pg.Release(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
