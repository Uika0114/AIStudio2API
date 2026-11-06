package requestdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func quotaFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.db")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := QuotaWindow(time.Now()).Add(time.Hour)
	since := QuotaWindow(now).Add(-24 * time.Hour)
	store.quotaSince = since
	if _, err = store.quotaConn.ExecContext(context.Background(), `UPDATE quota17_tracking SET observed_since=? WHERE id=1`, since.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return store, path, now
}
func quotaMembers() []api.QuotaMember {
	return []api.QuotaMember{{ID: "a", Label: "Account A", Tier: "Pro", Enabled: true, State: "ready"}, {ID: "b", Label: "Account B", Tier: "Ultra", Enabled: true, State: "cooldown"}}
}
func quotaSend(id, account string, at time.Time, total int64) aistudio.GenerationAttempt {
	attempt := aistudio.GenerationAttempt{ID: id, Revision: 2, AccountID: account, BaseModel: "gemini-4-argon", Channel: aistudio.ChannelPlayground, Sent: true, SentAt: at, StartedAt: at.Add(-time.Minute), Stage: "sending"}
	if total >= 0 {
		attempt.Stage = "upstream_finished"
		attempt.Usage = &aistudio.Usage{TotalTokens: total, InputTokens: total, CachedTokens: total / 2}
		attempt.UsageSource = "upstream"
	}
	return attempt
}
func quotaRead(t *testing.T, store *Store, members []api.QuotaMember, now time.Time) api.QuotaReport {
	t.Helper()
	report, err := store.QuotaSnapshot(context.Background(), members, now)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
func quotaValue(t *testing.T, value *int64, want int64) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("value=%v want=%d", value, want)
	}
}

func TestQuotaReportedUsageDeduplicatesAndColdReads(t *testing.T) {
	store, path, now := quotaFixture(t)
	a := quotaSend("root/attempt-1", "a", now, 125000)
	store.RecordQuotaAttempt(a)
	store.RecordQuotaAttempt(a)
	older := a
	older.Revision = 1
	older.Usage = &aistudio.Usage{TotalTokens: 999999}
	store.RecordQuotaAttempt(older)
	store.RecordQuotaAttempt(quotaSend("root/attempt-2", "a", now, 5000))
	store.RecordQuotaAttempt(quotaSend("root/attempt-3", "b", now, 70000))
	report := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Limit, 250)
	quotaValue(t, report.Requests.Used, 3)
	quotaValue(t, report.Requests.Remaining, 247)
	quotaValue(t, report.Tokens.Limit, 45000000)
	quotaValue(t, report.Tokens.Used, 200000)
	quotaValue(t, report.Tokens.Remaining, 44800000)
	if report.Unreported != 0 || report.InFlight != 0 || len(report.Accounts) != 2 {
		t.Fatalf("bad report: %+v", report)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cold := quotaRead(t, reopened, quotaMembers(), now)
	quotaValue(t, cold.Tokens.Used, 200000)
	quotaValue(t, cold.Requests.Remaining, 247)
	// Same local sequence after restart is another physical send, not a collision.
	reopened.RecordQuotaAttempt(quotaSend("root/attempt-1", "a", now, 100))
	quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Requests.Used, 4)
}

func TestQuota429LocksBothDimensionsUntil16WithoutInventingUsage(t *testing.T) {
	store, _, now := quotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("first", "a", now, 1234))
	limited := quotaSend("limit", "a", now, -1)
	limited.StatusCode = 429
	limited.Stage = "http_error"
	store.RecordQuotaAttempt(limited)
	report := quotaRead(t, store, quotaMembers(), now)
	a := report.Accounts[0]
	if !a.Exhausted429 {
		t.Fatal("429 state missing")
	}
	quotaValue(t, a.Tokens.Remaining, 0)
	quotaValue(t, a.Requests.Remaining, 0)
	quotaValue(t, a.Tokens.Used, 1234)
	quotaValue(t, a.Requests.Used, 2)
	quotaValue(t, report.Tokens.Remaining, 30000000)
	quotaValue(t, report.Requests.Remaining, 200)
	// A later successful send doesn't silently undo this window's conservative latch.
	store.RecordQuotaAttempt(quotaSend("after-minute-cooldown", "a", now, 100))
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Accounts[0].Tokens.Remaining, 0)
	next := QuotaWindow(now).Add(24 * time.Hour)
	fresh := quotaRead(t, store, quotaMembers(), next)
	quotaValue(t, fresh.Tokens.Remaining, 45000000)
	quotaValue(t, fresh.Requests.Remaining, 250)
	if fresh.Accounts[0].Exhausted429 {
		t.Fatal("429 leaked into new window")
	}
	limited.Revision++
	limited.Usage = &aistudio.Usage{TotalTokens: 10}
	limited.UsageSource = "upstream"
	store.RecordQuotaAttempt(limited)
	quotaValue(t, quotaRead(t, store, quotaMembers(), next).Tokens.Used, 0)
}

func TestQuotaCoarseMissingUsageAndScope(t *testing.T) {
	store, _, now := quotaFixture(t)
	pending := quotaSend("pending", "a", now, -1)
	store.RecordQuotaAttempt(pending)
	report := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 1)
	quotaValue(t, report.Tokens.Remaining, 45000000)
	if report.InFlight != 1 || report.Unreported != 1 {
		t.Fatalf("missing quality counts: %+v", report)
	}
	pending.Revision++
	pending.Stage = "timeout"
	store.RecordQuotaAttempt(pending)
	report = quotaRead(t, store, quotaMembers(), now)
	if report.InFlight != 0 || report.Unreported != 1 {
		t.Fatal("terminal missing usage was erased")
	}
	other := quotaSend("build", "a", now, 100)
	other.Channel = aistudio.ChannelBuild
	other.RateLimited = true
	store.RecordQuotaAttempt(other)
	other = quotaSend("flash", "a", now, 100)
	other.BaseModel = "gemini-3.8-flash"
	other.StatusCode = 429
	store.RecordQuotaAttempt(other)
	other = quotaSend("unsent", "a", now, 100)
	other.Sent = false
	store.RecordQuotaAttempt(other)
	report = quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 1)
	if report.Accounts[0].Exhausted429 {
		t.Fatal("other scope 429 polluted PG Argon")
	}
	pending.Revision++
	pending.Usage = &aistudio.Usage{TotalTokens: 7654}
	pending.UsageSource = "upstream"
	store.RecordQuotaAttempt(pending)
	report = quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Tokens.Used, 7654)
	quotaValue(t, report.Requests.Used, 1)
	if report.Unreported != 0 {
		t.Fatal("late usage didn't replace missing state")
	}
}

func TestQuotaInitialPartialMembersAndBoundary(t *testing.T) {
	store, _, now := quotaFixture(t)
	store.quotaSince = QuotaWindow(now).Add(time.Minute)
	report := quotaRead(t, store, quotaMembers(), now)
	if report.Tokens.Remaining != nil || report.Requests.Remaining != nil || report.Coverage != "partial" {
		t.Fatal("initial window invented full balance")
	}
	a := quotaSend("429", "a", now, -1)
	a.RateLimited = true
	a.Stage = "failed"
	store.RecordQuotaAttempt(a)
	quotaValue(t, quotaRead(t, store, quotaMembers()[:1], now).Tokens.Remaining, 0)
	next := QuotaWindow(now).Add(24 * time.Hour)
	members := append(quotaMembers(), api.QuotaMember{ID: "a", Label: "renamed duplicate", Tier: "Ultra", Enabled: true}, api.QuotaMember{ID: "off", Tier: "Ultra", Enabled: false}, api.QuotaMember{ID: "unknown", Tier: "Plus", Enabled: true})
	fresh := quotaRead(t, store, members, next)
	quotaValue(t, fresh.Requests.Limit, 250)
	if fresh.ExcludedAccounts != 1 || len(fresh.Accounts) != 3 {
		t.Fatalf("bad members: %+v", fresh)
	}
	for _, location := range []*time.Location{time.FixedZone("east", 8*3600), time.FixedZone("west", -8*3600), time.UTC} {
		if !QuotaWindow(next.In(location)).Equal(next) || !QuotaWindow(next.Add(-time.Nanosecond).In(location)).Equal(next.Add(-24*time.Hour)) {
			t.Fatal("16:00 boundary depends on timezone")
		}
	}
}

func TestQuotaConcurrentSnapshotsAndBoundedWriteFailure(t *testing.T) {
	store, _, now := quotaFixture(t)
	var wg sync.WaitGroup
	for i := 1; i <= 25; i++ {
		wg.Add(1)
		go func(rev int) {
			defer wg.Done()
			a := quotaSend("one", "a", now, int64(rev))
			a.Revision = uint64(rev)
			store.RecordQuotaAttempt(a)
		}(i)
	}
	wg.Wait()
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Tokens.Used, 25)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE quota17_tracking SET clean=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	store.RecordQuotaAttempt(quotaSend("busy", "a", now, 123))
	elapsed := time.Since(started)
	if err = store.flushQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if elapsed > time.Second {
		t.Fatalf("quota write stalled generation for %s", elapsed)
	}
	if store.quotaGap.Load() == 0 {
		t.Fatal("write failure silently lost coverage")
	}
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Tokens.Used, 25)
}

func TestQuotaObservationDoesNotWaitForSQLite(t *testing.T) {
	store, _, now := quotaFixture(t)
	store.quotaMu.Lock()
	done := make(chan struct{})
	go func() { store.RecordQuotaAttempt(quotaSend("nonblocking", "a", now, 100)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		store.quotaMu.Unlock()
		t.Fatal("display observer waited for SQLite")
	}
	store.quotaMu.Unlock()
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Tokens.Used, 100)
}

func TestQuotaBackpressureOnlyDegradesDisplay(t *testing.T) {
	store, _, now := quotaFixture(t)
	store.quotaMu.Lock()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			a := quotaSend(fmt.Sprintf("pressure-%d", i), "a", now, 1)
			store.RecordQuotaAttempt(a)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		store.quotaMu.Unlock()
		t.Fatal("full quota queue blocked observer")
	}
	store.quotaMu.Unlock()
	if store.quotaGap.Load() == 0 {
		t.Fatal("overflow lost data without a coverage signal")
	}
	report := quotaRead(t, store, quotaMembers(), now)
	if report.Coverage != "gap" {
		t.Fatal("overflow not reflected in read-only report")
	}
}

func TestQuotaUncleanRecoveryPreservesUsageWithoutGhostInFlight(t *testing.T) {
	store, path, now := quotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("unfinished", "a", now, -1))
	if err := store.flushQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE quota17_tracking SET clean=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report := quotaRead(t, reopened, quotaMembers(), time.Now())
	quotaValue(t, report.Requests.Used, 1)
	quotaValue(t, report.Tokens.Used, 0)
	if report.InFlight != 0 || report.Unreported != 1 || report.Coverage != "gap" {
		t.Fatalf("bad unclean recovery: %+v", report)
	}
}
