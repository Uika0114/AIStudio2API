package app

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func (requests *requestRegistry) observeQuotaAttempt(attempt aistudio.GenerationAttempt) {
	if requests == nil {
		return
	}
	if ledger := requests.quota.Load(); ledger != nil {
		ledger.RecordQuotaAttempt(attempt)
	}
}

func (manager *runtimeManager) Quota(ctx context.Context) (api.QuotaReport, error) {
	return manager.QuotaForModel(ctx, "gemini-4-argon")
}

func (manager *runtimeManager) QuotaForModel(ctx context.Context, model string) (api.QuotaReport, error) {
	switch model {
	case "gemini-4-argon", "gemini-4-carbon", "gemini-4-hydrogen":
		return manager.quotaForKnownModel(ctx, model, false)
	default:
		if !aistudio.IsGemini4QuotaModel(model) {
			return api.QuotaReport{}, api.ErrUnsupportedQuotaModel
		}
		return manager.quotaForKnownModel(ctx, model, true)
	}
}

func (manager *runtimeManager) quotaForKnownModel(ctx context.Context, model string, dynamic bool) (api.QuotaReport, error) {
	manager.mu.RLock()
	if dynamic {
		catalog, err := manager.current.admin.CatalogModels(ctx)
		if err != nil {
			manager.mu.RUnlock()
			return api.QuotaReport{}, err
		}
		if !slices.ContainsFunc(catalog, func(m aistudio.Model) bool { return aistudio.Gemini4PlaygroundQuotaBase(m) == model }) {
			manager.mu.RUnlock()
			return api.QuotaReport{}, api.ErrUnsupportedQuotaModel
		}
	}
	accounts, err := manager.current.admin.Accounts(ctx)
	ledger := manager.ledger
	manager.mu.RUnlock()
	if err != nil {
		return api.QuotaReport{}, err
	}
	members := make([]api.QuotaMember, 0, len(accounts))
	for _, account := range accounts {
		if dynamic && !slices.ContainsFunc(account.Models, func(id string) bool { return strings.TrimPrefix(id, "models/") == model }) {
			continue
		}
		members = append(members, api.QuotaMember{ID: account.ID, Label: account.Label, Tier: account.BenefitTier, State: account.State, Enabled: account.Enabled, BlockCode: account.BlockCode})
	}
	return ledger.QuotaSnapshotForModel(ctx, members, time.Now(), model)
}
