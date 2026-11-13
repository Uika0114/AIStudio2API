package requestdb

import (
	"context"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Defaults are estimates. Explicit upstream limits belong to the account and
// permission used for that send, not every account with the same subscription.
const quotaLimitsSchema = `CREATE TABLE IF NOT EXISTS quota_observed_limits (
 model TEXT NOT NULL, tier TEXT NOT NULL, account TEXT NOT NULL,
 dimension TEXT NOT NULL CHECK(dimension IN ('requests','tokens')),
 value INTEGER NOT NULL CHECK(value>=0 AND value<9007199254740992),
 observed_at INTEGER NOT NULL,
 PRIMARY KEY(model,tier,account,dimension));`

type quotaLimitUpdate struct {
	tier             string
	at               time.Time
	requests, tokens *int64
}

type quotaLimitKey struct {
	tier, account, dimension string
}

func quotaLimitObservation(attempt aistudio.GenerationAttempt) quotaLimitUpdate {
	if attempt.QuotaLimits == nil || attempt.QuotaLimits.ObservedAt.IsZero() ||
		(!attempt.RateLimited && attempt.StatusCode != 429) {
		return quotaLimitUpdate{}
	}
	tier := strings.ToLower(attempt.QuotaTier)
	if tier != "ultra" && tier != "pro" {
		return quotaLimitUpdate{}
	}
	copyLimit := func(value *int64) *int64 {
		if value == nil || *value < 0 || *value >= 1<<53 {
			return nil
		}
		return quotaInt(*value)
	}
	return quotaLimitUpdate{tier: tier, at: attempt.QuotaLimits.ObservedAt,
		requests: copyLimit(attempt.QuotaLimits.Requests), tokens: copyLimit(attempt.QuotaLimits.Tokens)}
}

func (store *Store) writeQuotaLimits(ctx context.Context, item quotaWrite) error {
	for _, value := range []struct {
		dimension string
		limit     *int64
	}{{"requests", item.limits.requests}, {"tokens", item.limits.tokens}} {
		if value.limit == nil {
			continue
		}
		// The evidence time is the physical send time. Older requests arriving
		// late and repeated snapshots cannot overwrite a newer observed limit.
		_, err := store.quotaConn.ExecContext(ctx, `INSERT INTO quota_observed_limits
 (model,tier,account,dimension,value,observed_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(model,tier,account,dimension) DO UPDATE SET
 value=excluded.value,observed_at=excluded.observed_at
 WHERE excluded.observed_at>quota_observed_limits.observed_at`,
			item.model, item.limits.tier, item.account, value.dimension, *value.limit, item.limits.at.UnixNano())
		if err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) readQuotaLimits(ctx context.Context, model string) (map[quotaLimitKey]int64, error) {
	rows, err := store.quotaConn.QueryContext(ctx, `SELECT tier,account,dimension,value FROM quota_observed_limits WHERE model=?`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	limits := make(map[quotaLimitKey]int64)
	for rows.Next() {
		var key quotaLimitKey
		var value int64
		if err := rows.Scan(&key.tier, &key.account, &key.dimension, &value); err != nil {
			return nil, err
		}
		limits[key] = value
	}
	return limits, rows.Err()
}
