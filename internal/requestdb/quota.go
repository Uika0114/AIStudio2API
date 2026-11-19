package requestdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

const quotaSchema = `CREATE TABLE IF NOT EXISTS quota17_tracking (
 id INTEGER PRIMARY KEY CHECK(id=1), observed_since INTEGER NOT NULL, clean INTEGER NOT NULL, gap_at INTEGER NOT NULL DEFAULT 0, closed_at INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS quota17_argon_attempts (
 epoch TEXT NOT NULL, id TEXT NOT NULL, revision INTEGER NOT NULL, account TEXT NOT NULL,
 quota_tier TEXT NOT NULL DEFAULT '',
 window INTEGER NOT NULL, sent_at INTEGER NOT NULL, tokens INTEGER NOT NULL,
 reported INTEGER NOT NULL, ended INTEGER NOT NULL, limited INTEGER NOT NULL,
 PRIMARY KEY(epoch,id));
 CREATE INDEX IF NOT EXISTS quota17_argon_window ON quota17_argon_attempts(window,account);
 CREATE TABLE IF NOT EXISTS quota17_history (
 window INTEGER PRIMARY KEY, until INTEGER NOT NULL, attempts INTEGER NOT NULL, skipped INTEGER NOT NULL);`

// QuotaWindow uses fixed UTC+8 16:00, independently of host timezone or DST.
func QuotaWindow(now time.Time) time.Time {
	utc := now.UTC()
	start := time.Date(utc.Year(), utc.Month(), utc.Day(), 8, 0, 0, 0, time.UTC)
	if utc.Before(start) {
		start = start.Add(-24 * time.Hour)
	}
	return start
}

func (store *Store) openQuota() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	quotaDB, err := sql.Open("sqlite", store.quotaDSN)
	if err != nil {
		return err
	}
	quotaDB.SetMaxOpenConns(1)
	conn, err := quotaDB.Conn(ctx)
	if err != nil {
		_ = quotaDB.Close()
		return err
	}
	fail := func(err error) error { _ = conn.Close(); _ = quotaDB.Close(); return err }
	// This connection is private to quota observation; don't change the normal writer's timeout.
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=25`); err != nil {
		return fail(err)
	}
	if _, err = conn.ExecContext(ctx, quotaLimitsSchema); err != nil {
		return fail(err)
	}
	now := time.Now().UTC()
	for _, scope := range []quotaScope{argonQuota, carbonQuota, hydrogenQuota} {
		if err = store.openQuotaScope(ctx, conn, scope, now); err != nil {
			return fail(err)
		}
	}
	store.quotaDB = quotaDB
	store.quotaConn = conn
	store.quotaEpoch = rand.Text()
	store.quotaWrites = make(chan quotaWrite, 256)
	store.quotaDone = make(chan struct{})
	go store.runQuota()
	return nil
}

func (store *Store) quotaFailure(scope quotaScope, at time.Time) {
	_, gap := store.quotaTracking(scope)
	value := at.UnixMilli()
	for old := gap.Load(); value > old; old = gap.Load() {
		if gap.CompareAndSwap(old, value) {
			break
		}
	}
}

// RecordQuotaAttempt never changes generation control flow. It records only numeric,
// credential/body-free facts from tracked physical Playground sends, not root HTTP usage.
func (store *Store) RecordQuotaAttempt(attempt aistudio.GenerationAttempt) {
	if store == nil || !attempt.Sent || attempt.Channel != aistudio.ChannelPlayground {
		return
	}
	scope, ok := quotaScopeForModel(strings.TrimPrefix(attempt.BaseModel, "models/"))
	if !ok {
		return
	}
	if attempt.SentAt.IsZero() || attempt.ID == "" || attempt.AccountID == "" || attempt.Revision > math.MaxInt64 {
		store.quotaFailure(scope, time.Now())
		return
	}
	tier := strings.ToLower(attempt.QuotaTier)
	if tier != "pro" && tier != "ultra" {
		tier = ""
	}
	limits := quotaLimitObservation(attempt)
	limited := 0
	if attempt.RateLimited || attempt.StatusCode == 429 {
		limited = 1 // Legacy or unclassified 429: the exhausted dimension is unknown.
		if limits.tokens != nil {
			limited = 2
		}
		if limits.requests != nil {
			limited = (limited &^ 1) | 4
		}
	}
	item := quotaWrite{model: scope.model, id: attempt.ID, account: attempt.AccountID, tier: tier, revision: attempt.Revision, sentAt: attempt.SentAt, ended: quotaTerminal(attempt.Stage), limited: limited, limits: limits}
	if attempt.Usage != nil && attempt.UsageSource == "upstream" && attempt.Usage.TotalTokens >= 0 && attempt.Usage.TotalTokens < 1<<53 {
		item.tokens = attempt.Usage.TotalTokens
		item.reported = true
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed || store.quotaWrites == nil {
		store.quotaFailure(scope, attempt.SentAt)
		return
	}
	select {
	case store.quotaWrites <- item:
	default:
		store.quotaFailure(scope, attempt.SentAt)
	}
}

type quotaWrite struct {
	model           string
	id, account     string
	tier            string
	revision        uint64
	sentAt          time.Time
	tokens          int64
	reported, ended bool
	limited         int // 1 unknown, 2 daily tokens, 4 daily requests; old boolean rows remain valid.
	barrier         chan struct{}
	limits          quotaLimitUpdate
}

func (store *Store) runQuota() {
	defer close(store.quotaDone)
	for item := range store.quotaWrites {
		if item.barrier != nil {
			close(item.barrier)
			continue
		}
		store.writeQuota(item)
	}
}

func (store *Store) writeQuota(item quotaWrite) {
	scope, ok := quotaScopeForModel(item.model)
	if !ok {
		return
	}
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err := store.quotaConn.ExecContext(ctx, `INSERT INTO `+scope.attempts+`
 (epoch,id,revision,account,quota_tier,window,sent_at,tokens,reported,ended,limited) VALUES(?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(epoch,id) DO UPDATE SET revision=excluded.revision,
 quota_tier=CASE WHEN `+scope.attempts+`.quota_tier='' THEN excluded.quota_tier ELSE `+scope.attempts+`.quota_tier END,
 tokens=CASE WHEN excluded.reported=1 THEN excluded.tokens ELSE `+scope.attempts+`.tokens END,
 reported=MAX(`+scope.attempts+`.reported,excluded.reported),
 ended=MAX(`+scope.attempts+`.ended,excluded.ended),
 limited=CASE WHEN excluded.limited>1 THEN ((`+scope.attempts+`.limited | excluded.limited) & 6)
 ELSE MAX(`+scope.attempts+`.limited,excluded.limited) END
 WHERE excluded.revision>`+scope.attempts+`.revision AND excluded.account=`+scope.attempts+`.account AND excluded.sent_at=`+scope.attempts+`.sent_at`,
		store.quotaEpoch, item.id, item.revision, item.account, item.tier, QuotaWindow(item.sentAt).UnixMilli(), item.sentAt.UnixMilli(), item.tokens, item.reported, item.ended, item.limited)
	if err == nil {
		err = store.writeQuotaLimits(ctx, item)
	}
	if err != nil {
		store.quotaFailure(scope, item.sentAt)
	}
}

func (store *Store) flushQuota(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	store.mu.RLock()
	if store.closed || store.quotaWrites == nil {
		store.mu.RUnlock()
		return errors.New("quota unavailable")
	}
	select {
	case store.quotaWrites <- quotaWrite{barrier: done}:
		store.mu.RUnlock()
	case <-ctx.Done():
		store.mu.RUnlock()
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func quotaTerminal(stage string) bool {
	switch stage {
	case "upstream_finished", "continued", "closed", "http_error", "failed", "timeout", "cancelled", "client_stop", "preparation_failed":
		return true
	}
	return false
}

func (store *Store) closeQuota() {
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	if store.quotaConn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, scope := range []quotaScope{argonQuota, carbonQuota, hydrogenQuota} {
		_, gap := store.quotaTracking(scope)
		if _, err := store.quotaConn.ExecContext(ctx, `UPDATE `+scope.tracking+` SET clean=1,gap_at=MAX(gap_at,?),closed_at=? WHERE id=1`, gap.Load(), time.Now().UnixMilli()); err != nil && store.log != nil {
			store.log("WARN", "额度观察未完成正常关闭；下次启动标记覆盖缺口")
		}
	}
	_ = store.quotaConn.Close()
	_ = store.quotaDB.Close()
	store.quotaConn = nil
}

type quotaUsage struct {
	requests, tokens, inFlight, unreported         int64
	unknownLimited, tokensLimited, requestsLimited bool
}

func quotaInt(value int64) *int64 { return &value }
func quotaCaps(tier string) (requests, tokens int64) {
	switch strings.ToLower(tier) {
	case "ultra":
		return 150, 20000000
	case "pro":
		return 50, 15000000
	}
	return 0, 0
}

func quotaBalance(limit, used int64, complete, limited, unknown bool) api.QuotaBalance {
	value := api.QuotaBalance{Limit: quotaInt(limit), Used: quotaInt(used)}
	if limited {
		value.Remaining = quotaInt(0)
	} else if complete && !unknown {
		value.Remaining = quotaInt(max(int64(0), limit-used))
	}
	return value
}

// QuotaSnapshot is a current-window estimate, not an admission or Google-balance API.
func (store *Store) QuotaSnapshot(ctx context.Context, members []api.QuotaMember, now time.Time) (api.QuotaReport, error) {
	return store.QuotaSnapshotForModel(ctx, members, now, argonQuota.model)
}

func (store *Store) QuotaSnapshotForModel(ctx context.Context, members []api.QuotaMember, now time.Time, model string) (api.QuotaReport, error) {
	if store == nil {
		return api.QuotaReport{}, errors.New("quota unavailable")
	}
	scope, ok := quotaScopeForModel(model)
	if !ok {
		return api.QuotaReport{}, errors.New("unsupported quota model")
	}
	since, gapState := store.quotaTracking(scope)
	if err := store.flushQuota(ctx); err != nil {
		return api.QuotaReport{}, err
	}
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	if store.quotaConn == nil {
		return api.QuotaReport{}, errors.New("quota unavailable")
	}
	from := QuotaWindow(now)
	report := api.QuotaReport{Channel: "playground", Model: scope.model, Method: "reported_usage_coarse_v1", PeriodFrom: from, ResetAt: from.Add(24 * time.Hour), UpdatedAt: now.UTC(), ObservedSince: *since, Coverage: "complete", Accounts: []api.QuotaAccount{}}
	complete := !since.After(from)
	if !complete {
		report.Coverage = "partial"
	}
	var until, attempts, skipped int64
	err := store.quotaConn.QueryRowContext(ctx, `SELECT until,attempts,skipped FROM `+scope.history+` WHERE window=?`, from.UnixMilli()).Scan(&until, &attempts, &skipped)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return api.QuotaReport{}, err
	}
	estimateKnown := complete
	if err == nil && attempts > 0 && until == since.UnixMilli() {
		report.History = &api.QuotaHistory{From: from, Until: time.UnixMilli(until).UTC(), Attempts: attempts, Skipped: skipped, TimeSource: "preparation_start"}
		if !complete {
			report.Coverage = "history_estimate"
			estimateKnown = true
		}
	}
	if gap := gapState.Load(); gap >= from.UnixMilli() && gap < report.ResetAt.UnixMilli() {
		report.Coverage = "gap"
	}
	rows, err := store.quotaConn.QueryContext(ctx, `SELECT account,quota_tier,COUNT(*),COALESCE(SUM(tokens),0),
 SUM(CASE WHEN ended=0 THEN 1 ELSE 0 END),SUM(CASE WHEN reported=0 THEN 1 ELSE 0 END),
 MAX((limited & 1)!=0),MAX((limited & 2)!=0),MAX((limited & 4)!=0)
 FROM `+scope.attempts+` WHERE window=? GROUP BY account,quota_tier`, from.UnixMilli())
	if err != nil {
		return api.QuotaReport{}, err
	}
	usage := make(map[string]map[string]quotaUsage)
	for rows.Next() {
		var id, tier string
		var u quotaUsage
		if err = rows.Scan(&id, &tier, &u.requests, &u.tokens, &u.inFlight, &u.unreported, &u.unknownLimited, &u.tokensLimited, &u.requestsLimited); err != nil {
			_ = rows.Close()
			return api.QuotaReport{}, err
		}
		if usage[id] == nil {
			usage[id] = make(map[string]quotaUsage)
		}
		usage[id][tier] = u
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return api.QuotaReport{}, err
	}
	limits, err := store.readQuotaLimits(ctx, scope.model)
	if err != nil {
		return api.QuotaReport{}, err
	}
	seen := make(map[string]bool)
	var reqLimit, tokLimit, reqUsed, tokUsed, reqRemaining, tokRemaining int64
	included, reqKnown, tokKnown := 0, true, true
	for _, member := range members {
		if !member.Enabled || member.ID == "" || seen[member.ID] {
			continue
		}
		seen[member.ID] = true
		// Carbon estimates are Ultra-only; Hydrogen also includes Pro, but no other tiers.
		if (scope == carbonQuota || scope == hydrogenQuota) && !strings.EqualFold(member.Tier, "ultra") &&
			!(scope == hydrogenQuota && strings.EqualFold(member.Tier, "pro")) {
			report.ExcludedAccounts++
			continue
		}
		var u quotaUsage
		for tier, observed := range usage[member.ID] {
			// Consumption remains an account-level coarse observation: a tier
			// change is not evidence that Google reset prior consumption.
			u.requests += observed.requests
			u.tokens += observed.tokens
			u.inFlight += observed.inFlight
			u.unreported += observed.unreported
			if tier == "" {
				u.unknownLimited = u.unknownLimited || observed.unknownLimited || observed.tokensLimited || observed.requestsLimited
			} else if tier == strings.ToLower(member.Tier) {
				u.unknownLimited = u.unknownLimited || observed.unknownLimited
				u.tokensLimited = u.tokensLimited || observed.tokensLimited
				u.requestsLimited = u.requestsLimited || observed.requestsLimited
			}
		}
		account := api.QuotaAccount{ID: member.ID, Label: member.Label, Tier: member.Tier, State: member.State, Exhausted429: u.unknownLimited || u.tokensLimited || u.requestsLimited, InFlight: u.inFlight, Unreported: u.unreported}
		account.BlockCode = member.BlockCode
		if account.BlockCode != "" {
			account.Requests.Used, account.Tokens.Used = quotaInt(u.requests), quotaInt(u.tokens)
			report.ExcludedAccounts++
			report.Accounts = append(report.Accounts, account)
			continue
		}
		r, t := quotaCaps(member.Tier)
		if r > 0 {
			if limit, ok := limits[quotaLimitKey{strings.ToLower(member.Tier), member.ID, "requests"}]; ok {
				r = limit
			}
			if limit, ok := limits[quotaLimitKey{strings.ToLower(member.Tier), member.ID, "tokens"}]; ok {
				t = limit
			}
			account.Included = true
			account.Requests = quotaBalance(r, u.requests, estimateKnown, u.requestsLimited, u.unknownLimited)
			account.Tokens = quotaBalance(t, u.tokens, estimateKnown, u.tokensLimited, u.unknownLimited)
			included++
			reqLimit += r
			tokLimit += t
			reqUsed += u.requests
			tokUsed += u.tokens
			if account.Requests.Remaining == nil {
				reqKnown = false
			} else {
				reqRemaining += *account.Requests.Remaining
			}
			if account.Tokens.Remaining == nil {
				tokKnown = false
			} else {
				tokRemaining += *account.Tokens.Remaining
			}
			report.InFlight += u.inFlight
			report.Unreported += u.unreported
		} else {
			report.ExcludedAccounts++
		}
		report.Accounts = append(report.Accounts, account)
	}
	sort.Slice(report.Accounts, func(i, j int) bool { return report.Accounts[i].ID < report.Accounts[j].ID })
	if included > 0 {
		report.Requests = api.QuotaBalance{Limit: quotaInt(reqLimit), Used: quotaInt(reqUsed)}
		report.Tokens = api.QuotaBalance{Limit: quotaInt(tokLimit), Used: quotaInt(tokUsed)}
		if reqKnown {
			report.Requests.Remaining = quotaInt(reqRemaining)
		}
		if tokKnown {
			report.Tokens.Remaining = quotaInt(tokRemaining)
		}
	}
	return report, nil
}
