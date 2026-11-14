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

func hydrogenQuotaFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	store, path, now := quotaFixture(t)
	store.quotaHydrogenSince = store.quotaSince
	if _, err := store.quotaConn.ExecContext(context.Background(), `UPDATE quota_hydrogen_tracking SET observed_since=? WHERE id=1`, store.quotaSince.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return store, path, now
}

func hydrogenSend(id, account string, at time.Time, total int64) aistudio.GenerationAttempt {
	attempt := quotaSend(id, account, at, total)
	attempt.BaseModel = "gemini-4-hydrogen"
	return attempt
}

func hydrogenRead(t *testing.T, store *Store, members []api.QuotaMember, now time.Time) api.QuotaReport {
	t.Helper()
	report, err := store.QuotaSnapshotForModel(context.Background(), members, now, "gemini-4-hydrogen")
	if err != nil {
		t.Fatal(err)
	}
	if report.Model != "gemini-4-hydrogen" || report.Channel != "playground" {
		t.Fatalf("wrong Hydrogen scope: %+v", report)
	}
	return report
}

func TestHydrogenQuotaSeparateModelTablesAndLegacyArgonRead(t *testing.T) {
	store, path, now := hydrogenQuotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("same-id", "b", now, 1200))
	store.RecordQuotaAttempt(carbonSend("same-id", "b", now, 2300))
	hydrogen := hydrogenSend("same-id", "b", now, 3400)
	store.RecordQuotaAttempt(hydrogen)
	store.RecordQuotaAttempt(hydrogen)
	older := hydrogen
	older.Revision--
	older.Usage = &aistudio.Usage{TotalTokens: 999999}
	store.RecordQuotaAttempt(older)
	store.RecordQuotaAttempt(hydrogenSend("pro-supply", "a", now, 5678))
	argon := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, argon.Tokens.Used, 1200)
	quotaValue(t, argon.Requests.Used, 1)
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Tokens.Used, 2300)
	report := hydrogenRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Tokens.Limit, 35000000)
	quotaValue(t, report.Tokens.Used, 9078)
	quotaValue(t, report.Tokens.Remaining, 35000000-9078)
	quotaValue(t, report.Requests.Limit, 200)
	quotaValue(t, report.Requests.Used, 2)
	if len(report.Accounts) != 2 || report.ExcludedAccounts != 0 {
		t.Fatalf("Hydrogen omitted an enabled Pro or Ultra: %+v", report)
	}
	pro := quotaAccount(t, report, "a")
	quotaValue(t, pro.Tokens.Limit, 15000000)
	quotaValue(t, pro.Tokens.Used, 5678)
	quotaValue(t, pro.Requests.Limit, 50)
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
	quotaValue(t, hydrogenRead(t, reopened, quotaMembers(), now).Tokens.Used, 9078)
	quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 1200)
	quotaValue(t, carbonRead(t, reopened, quotaMembers(), now).Tokens.Used, 2300)
	reopened.RecordQuotaAttempt(hydrogenSend("same-id", "b", now, 50))
	quotaValue(t, hydrogenRead(t, reopened, quotaMembers(), now).Requests.Used, 3)
}

func TestHydrogenQuotaProUltraMembershipAndDisableRestore(t *testing.T) {
	store, _, now := hydrogenQuotaFixture(t)
	store.RecordQuotaAttempt(hydrogenSend("ultra", "b", now, 800))
	store.RecordQuotaAttempt(hydrogenSend("pro", "a", now, 200))
	members := append(quotaMembers(),
		api.QuotaMember{ID: "b", Tier: "Ultra", Enabled: true},
		api.QuotaMember{ID: "off", Tier: "Ultra"},
		api.QuotaMember{ID: "unknown", Tier: "Plus", Enabled: true},
		api.QuotaMember{ID: "free", Tier: "Free", Enabled: true},
	)
	report := hydrogenRead(t, store, members, now)
	quotaValue(t, report.Requests.Limit, 200)
	quotaValue(t, report.Requests.Used, 2)
	if len(report.Accounts) != 2 || report.ExcludedAccounts != 2 {
		t.Fatal("duplicate, disabled, unknown or free account enlarged Hydrogen supply")
	}
	members[1].Enabled = false
	members[2].Enabled = false
	proOnly := hydrogenRead(t, store, members, now)
	quotaValue(t, proOnly.Tokens.Limit, 15000000)
	quotaValue(t, proOnly.Tokens.Used, 200)
	quotaValue(t, proOnly.Tokens.Remaining, 15000000-200)
	quotaValue(t, proOnly.Requests.Limit, 50)
	quotaValue(t, proOnly.Requests.Remaining, 49)
	if !quotaAccount(t, proOnly, "a").Included {
		t.Fatal("Pro-only Hydrogen pool was excluded")
	}
	carbon := carbonRead(t, store, members, now)
	if len(carbon.Accounts) != 0 || carbon.Tokens.Limit != nil {
		t.Fatalf("Pro-only pool enlarged Carbon supply: %+v", carbon)
	}
	members[0].Enabled = false
	empty := hydrogenRead(t, store, members, now)
	if len(empty.Accounts) != 0 || empty.Tokens.Limit != nil || empty.Requests.Remaining != nil {
		t.Fatalf("disabled or unsupported members enlarged Hydrogen supply: %+v", empty)
	}
	members[0].Enabled = true
	members[1].Enabled = true
	quotaValue(t, hydrogenRead(t, store, members, now).Tokens.Used, 1000)
}

func TestHydrogenQuota429AndLateUsageStayInOwnModelAndWindow(t *testing.T) {
	store, _, now := hydrogenQuotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("argon", "b", now, 1000))
	store.RecordQuotaAttempt(carbonSend("carbon", "b", now, 2000))
	store.RecordQuotaAttempt(hydrogenSend("measured", "b", now, 3000))
	limited := hydrogenSend("429", "b", now, -1)
	limited.StatusCode = 429
	limited.Stage = "http_error"
	store.RecordQuotaAttempt(limited)
	report := hydrogenRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Tokens.Used, 3000)
	quotaValue(t, report.Requests.Used, 2)
	quotaUnknown(t, report.Requests.Remaining)
	quotaUnknown(t, report.Tokens.Remaining)
	if !quotaAccount(t, report, "b").Exhausted429 || report.Unreported != 1 {
		t.Fatal("Hydrogen 429/missing-usage metadata missing")
	}
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Requests.Remaining, 199)
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Requests.Used, 1)
	if carbonRead(t, store, quotaMembers(), now).Accounts[0].Exhausted429 {
		t.Fatal("Hydrogen 429 affected Carbon")
	}
	store.RecordQuotaAttempt(hydrogenSend("after-minute-cooldown", "b", now, 10))
	quotaUnknown(t, hydrogenRead(t, store, quotaMembers(), now).Tokens.Remaining)
	next := QuotaWindow(now).Add(24 * time.Hour)
	fresh := hydrogenRead(t, store, quotaMembers(), next)
	quotaValue(t, fresh.Tokens.Remaining, 35000000)
	quotaValue(t, fresh.Requests.Remaining, 200)
	limited.Revision++
	limited.Usage = &aistudio.Usage{TotalTokens: 100}
	limited.UsageSource = "upstream"
	store.RecordQuotaAttempt(limited)
	quotaValue(t, hydrogenRead(t, store, quotaMembers(), next).Tokens.Used, 0)
	quotaValue(t, hydrogenRead(t, store, quotaMembers(), now).Tokens.Used, 3110)
	// The reverse direction must be isolated too.
	argon429 := quotaSend("argon429", "b", next, -1)
	argon429.StatusCode = 429
	store.RecordQuotaAttempt(argon429)
	quotaValue(t, hydrogenRead(t, store, quotaMembers(), next).Tokens.Remaining, 35000000)
}

func TestHydrogenQuotaPartialScopeAndMissingUsage(t *testing.T) {
	store, _, now := hydrogenQuotaFixture(t)
	store.quotaHydrogenSince = QuotaWindow(now).Add(time.Minute)
	pending := hydrogenSend("pending", "b", now, -1)
	store.RecordQuotaAttempt(pending)
	report := hydrogenRead(t, store, quotaMembers(), now)
	if report.Coverage != "partial" || report.Tokens.Remaining != nil || report.InFlight != 1 || report.Unreported != 1 {
		t.Fatalf("Hydrogen borrowed old Argon coverage: %+v", report)
	}
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Tokens.Remaining, 35000000)
	pending.Revision++
	pending.Stage = "cancelled"
	pending.Usage = &aistudio.Usage{TotalTokens: 7654}
	pending.UsageSource = "upstream"
	store.RecordQuotaAttempt(pending)
	store.RecordQuotaAttempt(pending)
	report = hydrogenRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 1)
	quotaValue(t, report.Tokens.Used, 7654)
	if report.Unreported != 0 || report.InFlight != 0 {
		t.Fatal("late Hydrogen usage/terminal state not replaced")
	}
	for _, channel := range []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelAssistant} {
		other := hydrogenSend(string(channel), "b", now, 999)
		other.Channel = channel
		other.StatusCode = 429
		store.RecordQuotaAttempt(other)
	}
	unsent := hydrogenSend("unsent", "b", now, 999)
	unsent.Sent = false
	store.RecordQuotaAttempt(unsent)
	quotaValue(t, hydrogenRead(t, store, quotaMembers(), now).Requests.Used, 1)
	if _, err := store.QuotaSnapshotForModel(context.Background(), quotaMembers(), now, "gemini-3.8-flash"); err == nil {
		t.Fatal("unsupported quota scope accepted")
	}
}

func TestHydrogenQuotaHistoryUsesItsOwnObservationStartAndDeduplicates(t *testing.T) {
	from := QuotaWindow(time.Now())
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	argon := historicalAttempt("argon", from.Add(10*time.Minute), 1200)
	hydrogen := historicalAttempt("hydrogen", from.Add(11*time.Minute), 3400)
	hydrogen.AccountID = "b"
	hydrogen.BaseModel = "gemini-4-hydrogen"
	store.Record(historicalRow("old", from.Add(20*time.Minute), argon, hydrogen))
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"quota17_tracking", "quota_hydrogen_tracking"} {
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
		report := hydrogenRead(t, reopened, quotaMembers(), now)
		quotaValue(t, report.Tokens.Used, 3400)
		quotaValue(t, report.Requests.Used, 1)
		if report.Coverage != "history_estimate" || report.History == nil || report.History.Attempts != 1 {
			t.Fatalf("Hydrogen history not independently imported: %+v", report)
		}
		quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 1200)
		if err = reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHydrogenQuotaUpgradeKeepsArgonTrackingAndStartsNewScope(t *testing.T) {
	store, path, now := hydrogenQuotaFixture(t)
	store.RecordQuotaAttempt(quotaSend("old", "b", now, 100))
	since := store.quotaSince
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy database: Hydrogen tables did not exist in the deployed binary.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"quota_hydrogen_attempts", "quota_hydrogen_tracking", "quota_hydrogen_history"} {
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
	if !reopened.quotaSince.Equal(since) || !reopened.quotaHydrogenSince.After(since) {
		t.Fatal("upgrade rewrote Argon start or borrowed its coverage")
	}
	quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 100)
	report := hydrogenRead(t, reopened, quotaMembers(), time.Now())
	if report.Coverage != "partial" || report.Tokens.Remaining != nil {
		t.Fatalf("new scope fabricated a full window: %+v", report)
	}
}
