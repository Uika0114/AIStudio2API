package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func strictFixturePool(t *testing.T) (*AccountPool, *Account) {
	t.Helper()
	model := Model{ID: "fixture-model", Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true, "function_declarations": true}}
	account := &Account{ID: "fixture-" + t.Name(), Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, modelAccessGeneration: 1}
	pool := NewAccountPool([]*Account{account}, 1)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	if err := pool.SetBuildCatalog(account.ID, []Model{model}); err != nil {
		t.Fatal(err)
	}
	return pool, account
}

func strictSelection(channel Channel) AccountSelection {
	return AccountSelection{ModelID: "fixture-model", Method: "generateContent", Channel: channel, StrictChannel: true}
}

func TestStrictChannelNeverFallsThroughSpecialSelection(t *testing.T) {
	pool := NewAccountPool(nil, 1)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	for _, item := range []struct {
		selection AccountSelection
		want      []Channel
	}{
		{strictSelection(ChannelBuild), []Channel{ChannelBuild}},
		{strictSelection(ChannelPlayground), []Channel{ChannelPlayground}},
		{AccountSelection{ModelID: "fixture-model", Method: "generateContent", Channel: ChannelBuild, StrictChannel: true, ResourceID: "file"}, nil},
		{AccountSelection{ModelID: "fixture-model", Method: "generateContent", Channel: ChannelBuild, StrictChannel: true, PlaygroundOnly: true}, nil},
		{AccountSelection{ModelID: "fixture-model", Method: "countTokens", Channel: ChannelBuild, StrictChannel: true}, nil},
	} {
		if got := pool.selectionChannelsLocked(item.selection); !reflect.DeepEqual(got, item.want) {
			t.Errorf("%+v: %v want %v", item.selection, got, item.want)
		}
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground})
	if got := pool.selectionChannelsLocked(strictSelection(ChannelBuild)); len(got) != 0 {
		t.Fatalf("disabled Build borrowed: %v", got)
	}
}

func TestStrictBuildCooldownDoesNotBorrowPlayground(t *testing.T) {
	pool, account := strictFixturePool(t)
	now := time.Now()
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, ChannelCooldownScope(ChannelBuild, "fixture-model"), 1, now, QuotaCooldown{Until: now.Add(time.Hour), Reason: "fixture quota"}); err != nil {
		t.Fatal(err)
	}
	lease, _, err := pool.TryAcquireFor(context.Background(), strictSelection(ChannelBuild))
	if lease != nil { // A cooling candidate may be reported as (nil, _, nil).
		if lease != nil {
			_ = lease.Release()
		}
		t.Fatalf("strict Build escaped cooldown: %v %v", lease, err)
	}
	lease, _, err = pool.TryAcquireFor(context.Background(), strictSelection(ChannelPlayground))
	if err != nil || lease == nil {
		t.Fatalf("PG was incorrectly cooled: %v", err)
	}
	if lease.Channel() != ChannelPlayground {
		t.Fatal("wrong PG lease")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStrictChannelsShareAccountConcurrency(t *testing.T) {
	pool, _ := strictFixturePool(t)
	lease, _, err := pool.TryAcquireFor(context.Background(), strictSelection(ChannelPlayground))
	if err != nil || lease == nil {
		t.Fatalf("PG: %v", err)
	}
	other, _, _ := pool.TryAcquireFor(context.Background(), strictSelection(ChannelBuild))
	if other != nil {
		_ = other.Release()
		_ = lease.Release()
		t.Fatal("channel split doubled account capacity")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	other, _, err = pool.TryAcquireFor(context.Background(), strictSelection(ChannelBuild))
	if err != nil || other == nil {
		t.Fatalf("Build after release: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStrictLeaseValidationWithPlaygroundOnlyFlag(t *testing.T) {
	pool, _ := strictFixturePool(t)
	lease, _, err := pool.TryAcquireFor(context.Background(), strictSelection(ChannelPlayground))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	selection := strictSelection(ChannelBuild)
	selection.PlaygroundOnly = true // previously bypassed channel checking
	if err := validateLeaseSelection(lease, selection); err == nil {
		t.Fatal("incompatible strict lease accepted")
	}
}

func TestGlobalQuotaSurvivesModelAndAuxiliarySuccess(t *testing.T) {
	pool, account := strictFixturePool(t)
	now := time.Now()
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, "", 1, now, QuotaCooldown{Global: true, Until: now.Add(time.Hour), Reason: "fixture global"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(account.ID, "fixture-model", 1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "build:fixture-model", "countTokens:fixture-model"} {
		if err := pool.ClearCooldownIfGeneration(account.ID, scope, 1, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if !account.runtime.Cooldowns[globalCooldownKey].Active(now) {
		t.Fatal("success erased global quota")
	}
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		lease, _, _ := pool.TryAcquireFor(context.Background(), strictSelection(channel))
		if lease != nil {
			if lease != nil {
				_ = lease.Release()
			}
			t.Fatalf("global restriction bypassed on %s", channel)
		}
	}
}

func TestAuxiliarySuccessCannotInvalidateLateGlobalQuota(t *testing.T) {
	pool, account := strictFixturePool(t)
	now := time.Now()
	if err := pool.ClearCooldownIfGeneration(account.ID, "", 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	until, err := pool.MarkQuotaCooldownIfGeneration(account.ID, "", 1, now, QuotaCooldown{Global: true, Until: now.Add(time.Hour), Reason: "late quota"})
	if err != nil || until.IsZero() {
		t.Fatalf("late generation quota was discarded: %v %v", until, err)
	}
	if err := pool.ClearCooldownIfGeneration(account.ID, "", 1, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, exists := account.runtime.Cooldowns[globalCooldownKey]; exists {
		t.Fatal("expired quota was not removed")
	}
}

func TestStrictPolicyCursorIsolation(t *testing.T) {
	pg, build := strictSelection(ChannelPlayground), strictSelection(ChannelBuild)
	auto := pg
	auto.StrictChannel = false
	auto.Channel = ""
	if selectionRoutingKey(pg) == selectionRoutingKey(build) || selectionRoutingKey(pg) == selectionRoutingKey(auto) {
		t.Fatal("strict cursors share auto state")
	}
	if selectionRoutingKey(auto) != selectionAccessScope(auto) {
		t.Fatal("legacy auto key changed")
	}
}

func TestStrictGenerationRefusesToolDroppingAndBudgetClamping(t *testing.T) {
	request, _ := NormalizeGenerateRequestModel(GenerateRequest{Model: "build/fixture-model"})
	request.Tools.Functions = []FunctionDeclaration{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}
	entry := modelEntry{model: Model{ID: "fixture-model", Capabilities: map[string]bool{}}, defaults: GenerationDefaults{MaxOutputTokens: 32768}}
	if err := validateStrictGenerateEntry(request, entry); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("tools silently dropped: %v", err)
	}
	entry.model.Capabilities["function_declarations"] = true
	limit := int64(65536)
	request.Config.MaxOutputTokens = &limit
	if err := validateStrictGenerateEntry(request, entry); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("budget silently clamped: %v", err)
	}
	entry.defaults.MaxOutputTokens = 65536
	if err := validateStrictGenerateEntry(request, entry); err != nil {
		t.Fatalf("native Build budget rejected using PG threshold: %v", err)
	}
	request.Selection = ModelSelection{} // legacy behavior remains delegated to original validation
	if err := validateStrictGenerateEntry(request, modelEntry{}); err != nil {
		t.Fatal(err)
	}
}

func TestStrictGenerationResourceAndContinuationSelection(t *testing.T) {
	build, _ := NormalizeGenerateRequestModel(GenerateRequest{Model: "build/fixture-model"})
	if _, err := GenerationAccountSelection(build, "file", 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Build accepted PG resource: %v", err)
	}
	selection, err := GenerationAccountSelection(build, "", 2)
	if err != nil || selection.PlaygroundOnly || selection.Channel != ChannelBuild {
		t.Fatalf("PG rounds overrode Build: %+v %v", selection, err)
	}
	pg, _ := NormalizeGenerateRequestModel(GenerateRequest{Model: "playground/fixture-model"})
	selection, err = GenerationAccountSelection(pg, "", 2)
	if err != nil || !selection.PlaygroundOnly || selection.Channel != ChannelPlayground {
		t.Fatalf("PG continuation lost pin: %+v %v", selection, err)
	}
}

func TestAntiTruncationRequiredKeepsOnlyRealRequiredChoices(t *testing.T) {
	request := GenerateRequest{Tools: Tools{Functions: []FunctionDeclaration{{Name: "real"}}, ToolConfig: ToolConfig{Mode: "required"}}}
	upstream, transport, err := PrepareAntiTruncation(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(upstream.Tools.ToolConfig.AllowedFunctionNames, []string{"real"}) {
		t.Fatalf("internal %s entered required choices: %v", transport.Name(), upstream.Tools.ToolConfig.AllowedFunctionNames)
	}
	if request.Tools.ToolConfig.AllowedFunctionNames != nil {
		t.Fatal("caller mutated")
	}
}
