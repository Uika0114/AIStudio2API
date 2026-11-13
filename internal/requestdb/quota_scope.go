package requestdb

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"time"
)

// Keep existing model tables unchanged so a program-only rollback cannot mix
// consumption. Each model has its own observation start and coverage.
type quotaScope struct {
	model, tracking, attempts, history string
}

var argonQuota = quotaScope{"gemini-4-argon", "quota17_tracking", "quota17_argon_attempts", "quota17_history"}
var carbonQuota = quotaScope{"gemini-4-carbon", "quota19_carbon_tracking", "quota19_carbon_attempts", "quota19_carbon_history"}
var hydrogenQuota = quotaScope{"gemini-4-hydrogen", "quota_hydrogen_tracking", "quota_hydrogen_attempts", "quota_hydrogen_history"}

func quotaScopeForModel(model string) (quotaScope, bool) {
	switch model {
	case argonQuota.model:
		return argonQuota, true
	case carbonQuota.model:
		return carbonQuota, true
	case hydrogenQuota.model:
		return hydrogenQuota, true
	default:
		return quotaScope{}, false
	}
}

func (scope quotaScope) schema() string {
	if scope == argonQuota {
		return quotaSchema
	}
	return strings.NewReplacer(
		argonQuota.tracking, scope.tracking,
		argonQuota.attempts, scope.attempts,
		"quota17_argon_window", strings.TrimSuffix(scope.attempts, "_attempts")+"_window",
		argonQuota.history, scope.history,
	).Replace(quotaSchema)
}

func (store *Store) quotaTracking(scope quotaScope) (*time.Time, *atomic.Int64) {
	if scope == carbonQuota {
		return &store.quotaCarbonSince, &store.quotaCarbonGap
	}
	if scope == hydrogenQuota {
		return &store.quotaHydrogenSince, &store.quotaHydrogenGap
	}
	return &store.quotaSince, &store.quotaGap
}

func (store *Store) openQuotaScope(ctx context.Context, conn *sql.Conn, scope quotaScope, now time.Time) error {
	if _, err := conn.ExecContext(ctx, scope.schema()); err != nil {
		return err
	}
	// Old rows have no trustworthy send-time tier; never infer one from current membership.
	var hasTier bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name='quota_tier')`, scope.attempts).Scan(&hasTier); err != nil {
		return err
	}
	if !hasTier {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE `+scope.attempts+` ADD COLUMN quota_tier TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO `+scope.tracking+`(id,observed_since,clean,gap_at) VALUES(1,?,1,0)`, now.UnixMilli()); err != nil {
		return err
	}
	var since, clean, gap, closedAt int64
	if err := conn.QueryRowContext(ctx, `SELECT observed_since,clean,gap_at,closed_at FROM `+scope.tracking+` WHERE id=1`).Scan(&since, &clean, &gap, &closedAt); err != nil {
		return err
	}
	if clean == 0 {
		gap = max(gap, now.UnixMilli())
	}
	if closedAt > 0 {
		var legacy bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE time>?)`, closedAt).Scan(&legacy); err != nil {
			return err
		}
		if legacy {
			gap = max(gap, now.UnixMilli())
		}
	}
	stale, err := conn.ExecContext(ctx, `UPDATE `+scope.attempts+` SET ended=1 WHERE ended=0`)
	if err != nil {
		return err
	}
	if count, _ := stale.RowsAffected(); count > 0 {
		gap = max(gap, now.UnixMilli())
	}
	if _, err = conn.ExecContext(ctx, `UPDATE `+scope.tracking+` SET clean=0,gap_at=? WHERE id=1`, gap); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM `+scope.attempts+` WHERE window < ?`, QuotaWindow(now.Add(-usageRetention)).UnixMilli()); err != nil {
		return err
	}
	observedSince := time.UnixMilli(since).UTC()
	if err = importQuotaHistoryForScope(ctx, conn, observedSince, now, scope); err != nil {
		return err
	}
	stateSince, stateGap := store.quotaTracking(scope)
	*stateSince = observedSince
	stateGap.Store(gap)
	return nil
}
