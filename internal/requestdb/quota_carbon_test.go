package requestdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func carbonQuotaFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	store, path, now := quotaFixture(t)
	store.quotaCarbonSince = store.quotaSince
	if _, err := store.quotaConn.ExecContext(context.Background(), `UPDATE quota19_carbon_tracking SET observed_since=? WHERE id=1`, store.quotaSince.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return store, path, now
}

func carbonSend(id, account string, at time.Time, total int64) aistudio.GenerationAttempt {
	attempt := quotaSend(id, account, at, total)
	attempt.BaseModel = "gemini-4-carbon"
	return attempt
}

func carbonRead(t *testing.T, store *Store, members []api.QuotaMember, now time.Time) api.QuotaReport {
	t.Helper()
	report, err := store.QuotaSnapshotForModel(context.Background(), members, now, "gemini-4-carbon")
	if err != nil {
		t.Fatal(err)
	}
	if report.Model != "gemini-4-carbon" || report.Channel != "playground" {
		t.Fatalf("wrong Carbon scope: %+v", report)
	}
	return report
}

func TestCarbonQuotaSeparateModelTablesAndLegacyArgonRead(t *testing.T) {
	store, path, now := carbonQuotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("same-id", "b", now, 1200))
	carbon := carbonSend("same-id", "b", now, 3400)
	store.RecordQuotaAttempt(carbon)
	store.RecordQuotaAttempt(carbon)
	older := carbon
	older.Revision--
	older.Usage = &aistudio.Usage{TotalTokens: 999999}
	store.RecordQuotaAttempt(older)
	store.RecordQuotaAttempt(carbonSend("pro-not-supply", "a", now, 5678))
	argon := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, argon.Tokens.Used, 1200)
	quotaValue(t, argon.Requests.Used, 1)
	report := carbonRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Tokens.Limit, 30000000)
	quotaValue(t, report.Tokens.Used, 3400)
	quotaValue(t, report.Tokens.Remaining, 30000000-3400)
	quotaValue(t, report.Requests.Limit, 200)
	quotaValue(t, report.Requests.Used, 1)
	if len(report.Accounts) != 1 || report.Accounts[0].ID != "b" || report.ExcludedAccounts != 1 {
		t.Fatalf("Carbon included non-Ultra: %+v", report)
	}
	// This is exactly the legacy reader's table, with no new model filter needed.
	var count, tokens int64
	if err := store.quotaConn.QueryRowContext(context.Background(), `SELECT COUNT(*),SUM(tokens) FROM quota17_argon_attempts`).Scan(&count, &tokens); err != nil || count != 1 || tokens != 1200 {
		t.Fatalf("old Argon reader polluted: count=%d tokens=%d err=%v", count, tokens, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	quotaValue(t, carbonRead(t, reopened, quotaMembers(), now).Tokens.Used, 3400)
	quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 1200)
	reopened.RecordQuotaAttempt(carbonSend("same-id", "b", now, 50))
	quotaValue(t, carbonRead(t, reopened, quotaMembers(), now).Requests.Used, 2)
}

func TestCarbonQuotaUltraOnlyMembershipAndDisableRestore(t *testing.T) {
	store, _, now := carbonQuotaFixture(t)
	store.RecordQuotaAttempt(carbonSend("used", "b", now, 800))
	members := append(quotaMembers(), api.QuotaMember{ID: "b", Tier: "Ultra", Enabled: true}, api.QuotaMember{ID: "off", Tier: "Ultra"}, api.QuotaMember{ID: "unknown", Tier: "Plus", Enabled: true})
	report := carbonRead(t, store, members, now)
	quotaValue(t, report.Requests.Limit, 200)
	quotaValue(t, report.Requests.Used, 1)
	if len(report.Accounts) != 1 {
		t.Fatal("duplicate, disabled or unknown account enlarged Carbon supply")
	}
	members[1].Enabled = false
	members[2].Enabled = false
	empty := carbonRead(t, store, members, now)
	if len(empty.Accounts) != 0 || empty.Tokens.Limit != nil || empty.Requests.Remaining != nil {
		t.Fatalf("no-Ultra pool is not empty: %+v", empty)
	}
	members[1].Enabled = true
	quotaValue(t, carbonRead(t, store, members, now).Tokens.Used, 800)
}

func TestCarbonQuota429AndLateUsageStayInOwnModelAndWindow(t *testing.T) {
	store, _, now := carbonQuotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("argon", "b", now, 1000))
	store.RecordQuotaAttempt(carbonSend("measured", "b", now, 3000))
	limited := carbonSend("429", "b", now, -1)
	limited.StatusCode = 429
	limited.Stage = "http_error"
	store.RecordQuotaAttempt(limited)
	report := carbonRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Tokens.Used, 3000)
	quotaValue(t, report.Requests.Used, 2)
	quotaValue(t, report.Requests.Remaining, 0)
	quotaValue(t, report.Tokens.Remaining, 0)
	if !report.Accounts[0].Exhausted429 || report.Unreported != 1 {
		t.Fatal("Carbon 429/missing-usage metadata missing")
	}
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Requests.Remaining, 249)
	store.RecordQuotaAttempt(carbonSend("after-minute-cooldown", "b", now, 10))
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Tokens.Remaining, 0)
	next := QuotaWindow(now).Add(24 * time.Hour)
	fresh := carbonRead(t, store, quotaMembers(), next)
	quotaValue(t, fresh.Tokens.Remaining, 30000000)
	quotaValue(t, fresh.Requests.Remaining, 200)
	limited.Revision++
	limited.Usage = &aistudio.Usage{TotalTokens: 100}
	limited.UsageSource = "upstream"
	store.RecordQuotaAttempt(limited)
	quotaValue(t, carbonRead(t, store, quotaMembers(), next).Tokens.Used, 0)
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Tokens.Used, 3110)
	// The reverse direction must be isolated too.
	argon429 := quotaSend("argon429", "b", next, -1)
	argon429.StatusCode = 429
	store.RecordQuotaAttempt(argon429)
	quotaValue(t, carbonRead(t, store, quotaMembers(), next).Tokens.Remaining, 30000000)
}

func TestCarbonQuotaPartialScopeAndMissingUsage(t *testing.T) {
	store, _, now := carbonQuotaFixture(t)
	store.quotaCarbonSince = QuotaWindow(now).Add(time.Minute)
	pending := carbonSend("pending", "b", now, -1)
	store.RecordQuotaAttempt(pending)
	report := carbonRead(t, store, quotaMembers(), now)
	if report.Coverage != "partial" || report.Tokens.Remaining != nil || report.InFlight != 1 || report.Unreported != 1 {
		t.Fatalf("Carbon borrowed old Argon coverage: %+v", report)
	}
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Tokens.Remaining, 45000000)
	pending.Revision++
	pending.Stage = "cancelled"
	pending.Usage = &aistudio.Usage{TotalTokens: 7654}
	pending.UsageSource = "upstream"
	store.RecordQuotaAttempt(pending)
	store.RecordQuotaAttempt(pending)
	report = carbonRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 1)
	quotaValue(t, report.Tokens.Used, 7654)
	if report.Unreported != 0 || report.InFlight != 0 {
		t.Fatal("late Carbon usage/terminal state not replaced")
	}
	for _, channel := range []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelAssistant} {
		other := carbonSend(string(channel), "b", now, 999)
		other.Channel = channel
		other.StatusCode = 429
		store.RecordQuotaAttempt(other)
	}
	unsent := carbonSend("unsent", "b", now, 999)
	unsent.Sent = false
	store.RecordQuotaAttempt(unsent)
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Requests.Used, 1)
	if _, err := store.QuotaSnapshotForModel(context.Background(), quotaMembers(), now, "gemini-3.8-flash"); err == nil {
		t.Fatal("unsupported quota scope accepted")
	}
}

func TestCarbonQuotaHistoryUsesItsOwnObservationStartAndDeduplicates(t *testing.T) {
	from := QuotaWindow(time.Now())
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	argon := historicalAttempt("argon", from.Add(10*time.Minute), 1200)
	carbon := historicalAttempt("carbon", from.Add(11*time.Minute), 3400)
	carbon.AccountID = "b"
	carbon.BaseModel = "gemini-4-carbon"
	store.Record(historicalRow("old", from.Add(20*time.Minute), argon, carbon))
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"quota17_tracking", "quota19_carbon_tracking"} {
		if _, err = db.Exec(`UPDATE `+table+` SET observed_since=?,closed_at=0 WHERE id=1`, from.Add(30*time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	for range 2 {
		reopened, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		now := from.Add(time.Hour)
		report := carbonRead(t, reopened, quotaMembers(), now)
		quotaValue(t, report.Tokens.Used, 3400)
		quotaValue(t, report.Requests.Used, 1)
		if report.Coverage != "history_estimate" || report.History == nil || report.History.Attempts != 1 {
			t.Fatalf("Carbon history not independently imported: %+v", report)
		}
		quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 1200)
		if err = reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCarbonQuotaUpgradeKeepsArgonTrackingAndStartsNewScope(t *testing.T) {
	store, path, now := carbonQuotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("old", "b", now, 100))
	since := store.quotaSince
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy database: Carbon tables did not exist in the deployed binary.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"quota19_carbon_attempts", "quota19_carbon_tracking", "quota19_carbon_history"} {
		if _, err = db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.quotaSince.Equal(since) || !reopened.quotaCarbonSince.After(since) {
		t.Fatal("upgrade rewrote Argon start or borrowed its coverage")
	}
	quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 100)
	report := carbonRead(t, reopened, quotaMembers(), time.Now())
	if report.Coverage != "partial" || report.Tokens.Remaining != nil {
		t.Fatalf("new scope fabricated a full window: %+v", report)
	}
}
