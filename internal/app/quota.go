package app

import (
	"context"
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
	manager.mu.RLock()
	accounts, err := manager.current.admin.Accounts(ctx)
	ledger := manager.ledger
	manager.mu.RUnlock()
	if err != nil {
		return api.QuotaReport{}, err
	}
	members := make([]api.QuotaMember, 0, len(accounts))
	for _, account := range accounts {
		members = append(members, api.QuotaMember{ID: account.ID, Label: account.Label, Tier: account.BenefitTier, State: account.State, Enabled: account.Enabled})
	}
	return ledger.QuotaSnapshot(ctx, members, time.Now())
}
