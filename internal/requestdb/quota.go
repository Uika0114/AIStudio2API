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
	if _, err = conn.ExecContext(ctx, quotaSchema); err != nil {
		return fail(err)
	}
	now := time.Now().UTC()
	if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO quota17_tracking(id,observed_since,clean,gap_at) VALUES(1,?,1,0)`, now.UnixMilli()); err != nil {
		return fail(err)
	}
	var since, clean, gap, closedAt int64
	if err = conn.QueryRowContext(ctx, `SELECT observed_since,clean,gap_at,closed_at FROM quota17_tracking WHERE id=1`).Scan(&since, &clean, &gap, &closedAt); err != nil {
		return fail(err)
	}
	if clean == 0 {
		gap = max(gap, now.UnixMilli())
	}
	if closedAt > 0 {
		var legacy bool
		if err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE time>?)`, closedAt).Scan(&legacy); err != nil {
			return fail(err)
		}
		if legacy {
			gap = max(gap, now.UnixMilli())
		}
	}
	stale, err := conn.ExecContext(ctx, `UPDATE quota17_argon_attempts SET ended=1 WHERE ended=0`)
	if err != nil {
		return fail(err)
	}
	if count, _ := stale.RowsAffected(); count > 0 {
		gap = max(gap, now.UnixMilli())
	}
	if _, err = conn.ExecContext(ctx, `UPDATE quota17_tracking SET clean=0,gap_at=? WHERE id=1`, gap); err != nil {
		return fail(err)
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM quota17_argon_attempts WHERE window < ?`, QuotaWindow(now.Add(-usageRetention)).UnixMilli()); err != nil {
		return fail(err)
	}
	if err = importQuotaHistory(ctx, conn, time.UnixMilli(since).UTC(), now); err != nil {
		return fail(err)
	}
	store.quotaDB = quotaDB
	store.quotaConn = conn
	store.quotaSince = time.UnixMilli(since).UTC()
	store.quotaEpoch = rand.Text()
	store.quotaGap.Store(gap)
	store.quotaWrites = make(chan quotaWrite, 256)
	store.quotaDone = make(chan struct{})
	go store.runQuota()
	return nil
}

func (store *Store) quotaFailure(at time.Time) {
	value := at.UnixMilli()
	for old := store.quotaGap.Load(); value > old; old = store.quotaGap.Load() {
		if store.quotaGap.CompareAndSwap(old, value) {
			break
		}
	}
}

// RecordQuotaAttempt never changes generation control flow. It records only numeric,
// credential/body-free facts from physical PG Argon sends, not root HTTP usage.
func (store *Store) RecordQuotaAttempt(attempt aistudio.GenerationAttempt) {
	if store == nil || !attempt.Sent || attempt.Channel != aistudio.ChannelPlayground || strings.TrimPrefix(attempt.BaseModel, "models/") != "gemini-4-argon" {
		return
	}
	if attempt.SentAt.IsZero() || attempt.ID == "" || attempt.AccountID == "" || attempt.Revision > math.MaxInt64 {
		store.quotaFailure(time.Now())
		return
	}
	item := quotaWrite{id: attempt.ID, account: attempt.AccountID, revision: attempt.Revision, sentAt: attempt.SentAt, ended: quotaTerminal(attempt.Stage), limited: attempt.RateLimited || attempt.StatusCode == 429}
	if attempt.Usage != nil && attempt.UsageSource == "upstream" && attempt.Usage.TotalTokens >= 0 && attempt.Usage.TotalTokens < 1<<53 {
		item.tokens = attempt.Usage.TotalTokens
		item.reported = true
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed || store.quotaWrites == nil {
		store.quotaFailure(attempt.SentAt)
		return
	}
	select {
	case store.quotaWrites <- item:
	default:
		store.quotaFailure(attempt.SentAt)
	}
}

type quotaWrite struct {
	id, account              string
	revision                 uint64
	sentAt                   time.Time
	tokens                   int64
	reported, ended, limited bool
	barrier                  chan struct{}
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
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err := store.quotaConn.ExecContext(ctx, `INSERT INTO quota17_argon_attempts
 (epoch,id,revision,account,window,sent_at,tokens,reported,ended,limited) VALUES(?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(epoch,id) DO UPDATE SET revision=excluded.revision,
 tokens=CASE WHEN excluded.reported=1 THEN excluded.tokens ELSE quota17_argon_attempts.tokens END,
 reported=MAX(quota17_argon_attempts.reported,excluded.reported),
 ended=MAX(quota17_argon_attempts.ended,excluded.ended),
 limited=MAX(quota17_argon_attempts.limited,excluded.limited)
 WHERE excluded.revision>quota17_argon_attempts.revision AND excluded.account=quota17_argon_attempts.account AND excluded.sent_at=quota17_argon_attempts.sent_at`,
		store.quotaEpoch, item.id, item.revision, item.account, QuotaWindow(item.sentAt).UnixMilli(), item.sentAt.UnixMilli(), item.tokens, item.reported, item.ended, item.limited)
	if err != nil {
		store.quotaFailure(item.sentAt)
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
	if _, err := store.quotaConn.ExecContext(ctx, `UPDATE quota17_tracking SET clean=1,gap_at=MAX(gap_at,?),closed_at=? WHERE id=1`, store.quotaGap.Load(), time.Now().UnixMilli()); err != nil && store.log != nil {
		store.log("WARN", "额度观察未完成正常关闭；下次启动标记覆盖缺口")
	}
	_ = store.quotaConn.Close()
	_ = store.quotaDB.Close()
	store.quotaConn = nil
}

type quotaUsage struct {
	requests, tokens, inFlight, unreported int64
	limited                                bool
}

func quotaInt(value int64) *int64 { return &value }
func quotaCaps(tier string) (requests, tokens int64) {
	switch strings.ToLower(tier) {
	case "ultra":
		return 200, 30000000
	case "pro":
		return 50, 15000000
	}
	return 0, 0
}

func quotaBalance(limit, used int64, complete, limited bool) api.QuotaBalance {
	value := api.QuotaBalance{Limit: quotaInt(limit), Used: quotaInt(used)}
	if limited {
		value.Remaining = quotaInt(0)
	} else if complete {
		value.Remaining = quotaInt(max(int64(0), limit-used))
	}
	return value
}

// QuotaSnapshot is a current-window estimate, not an admission or Google-balance API.
func (store *Store) QuotaSnapshot(ctx context.Context, members []api.QuotaMember, now time.Time) (api.QuotaReport, error) {
	if store == nil {
		return api.QuotaReport{}, errors.New("quota unavailable")
	}
	if err := store.flushQuota(ctx); err != nil {
		return api.QuotaReport{}, err
	}
	store.quotaMu.Lock()
	defer store.quotaMu.Unlock()
	if store.quotaConn == nil {
		return api.QuotaReport{}, errors.New("quota unavailable")
	}
	from := QuotaWindow(now)
	report := api.QuotaReport{Channel: "playground", Model: "gemini-4-argon", Method: "reported_usage_coarse_v1", PeriodFrom: from, ResetAt: from.Add(24 * time.Hour), UpdatedAt: now.UTC(), ObservedSince: store.quotaSince, Coverage: "complete", Accounts: []api.QuotaAccount{}}
	complete := !store.quotaSince.After(from)
	if !complete {
		report.Coverage = "partial"
	}
	var until, attempts, skipped int64
	err := store.quotaConn.QueryRowContext(ctx, `SELECT until,attempts,skipped FROM quota17_history WHERE window=?`, from.UnixMilli()).Scan(&until, &attempts, &skipped)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return api.QuotaReport{}, err
	}
	estimateKnown := complete
	if err == nil && attempts > 0 && until == store.quotaSince.UnixMilli() {
		report.History = &api.QuotaHistory{From: from, Until: time.UnixMilli(until).UTC(), Attempts: attempts, Skipped: skipped, TimeSource: "preparation_start"}
		if !complete {
			report.Coverage = "history_estimate"
			estimateKnown = true
		}
	}
	if gap := store.quotaGap.Load(); gap >= from.UnixMilli() && gap < report.ResetAt.UnixMilli() {
		report.Coverage = "gap"
	}
	rows, err := store.quotaConn.QueryContext(ctx, `SELECT account,COUNT(*),COALESCE(SUM(tokens),0),SUM(CASE WHEN ended=0 THEN 1 ELSE 0 END),SUM(CASE WHEN reported=0 THEN 1 ELSE 0 END),MAX(limited) FROM quota17_argon_attempts WHERE window=? GROUP BY account`, from.UnixMilli())
	if err != nil {
		return api.QuotaReport{}, err
	}
	usage := make(map[string]quotaUsage)
	for rows.Next() {
		var id string
		var u quotaUsage
		var limited int
		if err = rows.Scan(&id, &u.requests, &u.tokens, &u.inFlight, &u.unreported, &limited); err != nil {
			_ = rows.Close()
			return api.QuotaReport{}, err
		}
		u.limited = limited != 0
		usage[id] = u
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return api.QuotaReport{}, err
	}
	seen := make(map[string]bool)
	var reqLimit, tokLimit, reqUsed, tokUsed, reqRemaining, tokRemaining int64
	included, allKnown := 0, true
	for _, member := range members {
		if !member.Enabled || member.ID == "" || seen[member.ID] {
			continue
		}
		seen[member.ID] = true
		u := usage[member.ID]
		account := api.QuotaAccount{ID: member.ID, Label: member.Label, Tier: member.Tier, State: member.State, Exhausted429: u.limited, InFlight: u.inFlight, Unreported: u.unreported}
		r, t := quotaCaps(member.Tier)
		if r > 0 {
			account.Included = true
			account.Requests = quotaBalance(r, u.requests, estimateKnown, u.limited)
			account.Tokens = quotaBalance(t, u.tokens, estimateKnown, u.limited)
			included++
			reqLimit += r
			tokLimit += t
			reqUsed += u.requests
			tokUsed += u.tokens
			if account.Requests.Remaining == nil || account.Tokens.Remaining == nil {
				allKnown = false
			} else {
				reqRemaining += *account.Requests.Remaining
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
		if allKnown {
			report.Requests.Remaining = quotaInt(reqRemaining)
			report.Tokens.Remaining = quotaInt(tokRemaining)
		}
	}
	return report, nil
}
