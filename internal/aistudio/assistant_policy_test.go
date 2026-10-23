package aistudio

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func assistantPolicyFixture(t *testing.T) (*AccountPool, *Account) {
	t.Helper()
	model := Model{ID: "gemini-4-argon", Methods: []string{"generateContent", "countTokens"}, InputTokenLimit: 1048576, OutputTokenLimit: 262144, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true, "output_continuation": true, "capability_code_48": true}, AccessModes: []int64{}}
	account := &Account{ID: "fixture-assistant-policy", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, modelAccessGeneration: 1}
	pool := NewAccountPool([]*Account{account}, 1)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild, ChannelAssistant})
	if err := pool.SetBuildCatalog(account.ID, []Model{model}); err != nil {
		t.Fatal(err)
	}
	return pool, account
}

func TestAssistantPolicyStrictOnlyAndDisabledGuard(t *testing.T) {
	pool, account := assistantPolicyFixture(t)
	selection := AccountSelection{ModelID: account.Models[0].ID, Method: "generateContent"}
	if got := pool.selectionChannelsLocked(selection); !reflect.DeepEqual(got, []Channel{ChannelPlayground, ChannelBuild}) {
		t.Fatalf("auto admitted assistant: %v", got)
	}
	selection.Channel = ChannelAssistant
	selection.StrictChannel = true
	if got := pool.selectionChannelsLocked(selection); !reflect.DeepEqual(got, []Channel{ChannelAssistant}) {
		t.Fatalf("strict route: %v", got)
	}
	selection.ResourceID = "fixture-file"
	if got := pool.selectionChannelsLocked(selection); len(got) != 0 {
		t.Fatal("assistant borrowed PG resource")
	}
	selection.ResourceID = ""
	selection.Method = "countTokens"
	if got := pool.selectionChannelsLocked(selection); len(got) != 0 {
		t.Fatal("assistant selected for auxiliary method")
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	selection.Method = "generateContent"
	if got := pool.selectionChannelsLocked(selection); len(got) != 0 {
		t.Fatal("disabled route fell back")
	}
}

func TestAssistantPolicySeparateCooldownsAndSharedCapacity(t *testing.T) {
	pool, account := assistantPolicyFixture(t)
	now := time.Now()
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, ChannelCooldownScope(ChannelBuild, "gemini-4-argon"), 1, now, QuotaCooldown{Until: now.Add(time.Hour), Reason: "fixture Build quota"}); err != nil {
		t.Fatal(err)
	}
	selection := AccountSelection{ModelID: "gemini-4-argon", Method: "generateContent", Channel: ChannelAssistant, StrictChannel: true}
	lease, _, err := pool.TryAcquireFor(context.Background(), selection)
	if err != nil || lease == nil {
		t.Fatalf("Build quota damaged assistant: %v", err)
	}
	selection.Channel = ChannelPlayground
	other, _, _ := pool.TryAcquireFor(context.Background(), selection)
	if other != nil {
		_ = other.Release()
		t.Fatal("third channel doubled account capacity")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, ChannelCooldownScope(ChannelAssistant, "gemini-4-argon"), 1, now, QuotaCooldown{Until: now.Add(time.Hour), Reason: "fixture assistant quota"}); err != nil {
		t.Fatal(err)
	}
	lease, _, err = pool.TryAcquireFor(context.Background(), selection)
	if err != nil || lease == nil {
		t.Fatalf("assistant quota damaged PG: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	selection.Channel = ChannelAssistant
	if lease, _, _ := pool.TryAcquireFor(context.Background(), selection); lease != nil {
		_ = lease.Release()
		t.Fatal("assistant escaped its own cooldown")
	}
}

func TestAssistantPolicyGlobalQuotaStillShared(t *testing.T) {
	pool, account := assistantPolicyFixture(t)
	now := time.Now()
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, "", 1, now, QuotaCooldown{Global: true, Until: now.Add(time.Hour), Reason: "fixture account global"}); err != nil {
		t.Fatal(err)
	}
	selection := AccountSelection{ModelID: "gemini-4-argon", Method: "generateContent", Channel: ChannelAssistant, StrictChannel: true}
	if lease, _, _ := pool.TryAcquireFor(context.Background(), selection); lease != nil {
		_ = lease.Release()
		t.Fatal("assistant bypassed account global quota")
	}
}

func TestAssistantPolicyCatalogAndModelMetadata(t *testing.T) {
	pool, account := assistantPolicyFixture(t)
	models := pool.PublicModels(account.Models)
	if len(models) != 1 || !reflect.DeepEqual(models[0].Channels, []string{"playground", "build", "assistant"}) {
		t.Fatalf("catalog: %+v", models)
	}
	variant := AssistantModelVariant(models[0])
	if variant.Capabilities["function_declarations"] != assistantFunctionToolsImplemented() || variant.Capabilities["output_continuation"] || variant.OutputTokenLimit != 0 || variant.InputTokenLimit != 0 {
		t.Fatalf("borrowed unsupported capabilities: %+v", variant)
	}
	if !models[0].Capabilities["function_declarations"] || !models[0].Capabilities["output_continuation"] {
		t.Fatal("mutated PG capabilities")
	}
	if got := models[0].ChannelTokenLimits[ChannelAssistant]; got.Input != 0 || got.Output != 0 {
		t.Fatal("invented assistant token bounds")
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	if got := pool.PublicModels(account.Models)[0].Channels; !reflect.DeepEqual(got, []string{"playground", "build"}) {
		t.Fatalf("disabled assistant listed: %v", got)
	}
}

func TestAssistantPolicyGenerationSelectionNeverBorrowsContinuation(t *testing.T) {
	request, err := NormalizeGenerateRequestModel(GenerateRequest{Model: "assistant/gemini-4-argon"})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := GenerationAccountSelection(request, "", 8)
	if err != nil || selection.PlaygroundOnly || selection.Channel != ChannelAssistant {
		t.Fatalf("PG continuation took route: %+v %v", selection, err)
	}
	if _, err := GenerationAccountSelection(request, "fixture-file", 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("file route accepted: %v", err)
	}
}

func TestAssistantPolicyCooldownScopeRoundTrip(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild, ChannelAssistant} {
		key := ChannelCooldownScope(channel, "gemini-4-argon")
		got, base, scoped := ParseChannelCooldownScope(key)
		if got != channel || base != "gemini-4-argon" || scoped != (channel != ChannelPlayground) {
			t.Fatalf("%q: %s %q %v", key, got, base, scoped)
		}
	}
}
