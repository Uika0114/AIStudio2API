package requestdb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func dynamicQuotaScope(model string) quotaScope {
	prefix := fmt.Sprintf("quota_g4_%x", sha256.Sum256([]byte(model)))
	return quotaScope{model, prefix + "_tracking", prefix + "_attempts", prefix + "_history"}
}

func (s quotaScope) dynamic() bool { return s != argonQuota && s != carbonQuota && s != hydrogenQuota }

type dynamicQuotaState struct {
	since  time.Time
	gap    atomic.Int64
	opened bool
}

func (store *Store) dynamicQuotaState(scope quotaScope) *dynamicQuotaState {
	store.quotaDynamicMu.Lock()
	defer store.quotaDynamicMu.Unlock()
	if state := store.quotaDynamic[scope]; state != nil {
		return state
	}
	if len(store.quotaDynamic) >= aistudio.MaxGemini4QuotaModels {
		return nil
	}
	if store.quotaDynamic == nil {
		store.quotaDynamic = make(map[quotaScope]*dynamicQuotaState)
	}
	state := &dynamicQuotaState{}
	store.quotaDynamic[scope] = state
	return state
}

func (store *Store) quotaScopesForClose() []quotaScope {
	scopes := []quotaScope{argonQuota, carbonQuota, hydrogenQuota}
	store.quotaDynamicMu.Lock()
	defer store.quotaDynamicMu.Unlock()
	for scope, state := range store.quotaDynamic {
		if state.opened {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

// Caller holds quotaMu; discovery never puts DDL on the generation path.
func (store *Store) ensureDynamicQuota(ctx context.Context, scope quotaScope, now time.Time) error {
	if !scope.dynamic() {
		return nil
	}
	state := store.dynamicQuotaState(scope)
	if state == nil {
		return fmt.Errorf("quota model limit reached")
	}
	if state.opened {
		return nil
	}
	var existing bool
	var count int
	if err := store.quotaConn.QueryRowContext(ctx, `SELECT COALESCE(MAX(name = ?), 0), COUNT(*) FROM sqlite_master WHERE type='table' AND name GLOB 'quota_g4_*_tracking'`, scope.tracking).Scan(&existing, &count); err != nil {
		return err
	}
	if !existing && count >= aistudio.MaxGemini4QuotaModels {
		return fmt.Errorf("quota model limit reached")
	}
	if err := store.openQuotaScopeAt(ctx, store.quotaConn, scope, now, store.quotaOpenedAt); err != nil {
		return err
	}
	state.opened = true
	return nil
}
