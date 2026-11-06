package requestdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func historicalAttempt(id string, at time.Time, total int64) api.RequestAttempt {
	sent := true
	a := api.RequestAttempt{Kind: api.AttemptKindGeneration, ID: id, Revision: 3, Account: "old editable label", AccountID: "a", BaseModel: "gemini-4-argon", Channel: "playground", Sent: &sent, StartedAt: &at, Stage: "upstream_finished", StatusCode: 200, Error: "PRIVATE_HISTORY_SENTINEL"}
	if total >= 0 {
		a.Usage = &aistudio.Usage{TotalTokens: total}
		a.UsageSource = "upstream"
	}
	return a
}
func historicalRow(id string, at time.Time, attempts ...api.RequestAttempt) Row {
	return Row{ID: id, Time: at, Protocol: "openai-chat", Path: "/v1/chat/completions", Model: "root-model-is-not-the-attempt-scope", Account: "root-final-account-is-not-identity", Channel: "build", Status: 200, State: "completed", Attempts: attempts}
}
func historyFixture(t *testing.T, rows []Row, live *aistudio.GenerationAttempt) (*Store, string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		store.Record(row)
	}
	if live != nil {
		store.RecordQuotaAttempt(*live)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	from := QuotaWindow(time.Now())
	_, err = conn.Exec(`UPDATE quota17_tracking SET observed_since=?,closed_at=0 WHERE id=1`, from.Add(30*time.Minute).UnixMilli())
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path, from.Add(time.Hour)
}
func TestQuotaHistoryBootstrapsAndReplaysWithoutDoubleCounting(t *testing.T) {
	from := QuotaWindow(time.Now())
	a := historicalAttempt("physical-1", from.Add(10*time.Minute), 125000)
	older := a
	older.Revision = 2
	older.Usage = &aistudio.Usage{TotalTokens: 999999}
	store, path, now := historyFixture(t, []Row{historicalRow("old-1", from.Add(20*time.Minute), a, older), historicalRow("old-2", from.Add(21*time.Minute), a)}, nil)
	report := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 2)
	quotaValue(t, report.Requests.Remaining, 248)
	quotaValue(t, report.Tokens.Used, 250000)
	quotaValue(t, report.Tokens.Remaining, 44750000)
	if report.Coverage != "history_estimate" || report.History == nil || report.History.Attempts != 2 || !report.ObservedSince.Equal(from.Add(30*time.Minute)) {
		t.Fatalf("history coverage wrong: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "PRIVATE_HISTORY_SENTINEL") || strings.Contains(string(encoded), "old editable label") {
		t.Fatal("history private fields leaked")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	quotaValue(t, quotaRead(t, reopened, quotaMembers(), now).Tokens.Used, 250000)
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	// A higher historical revision replaces an existing amount, not another debit.
	a.Revision++
	a.Usage.TotalTokens = 150000
	payload, _ := json.Marshal([]api.RequestAttempt{a})
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(`UPDATE requests SET attempts=? WHERE id='old-1'`, string(payload))
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	late, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	updated := quotaRead(t, late, quotaMembers(), now)
	quotaValue(t, updated.Requests.Used, 2)
	quotaValue(t, updated.Tokens.Used, 275000)
	fresh := quotaRead(t, late, quotaMembers(), from.Add(24*time.Hour))
	quotaValue(t, fresh.Requests.Used, 0)
	quotaValue(t, fresh.Tokens.Remaining, 45000000)
	if fresh.History != nil {
		t.Fatal("historical window leaked into next day")
	}
}
func TestQuotaHistoryCutoffDoesNotDoubleExistingLiveRows(t *testing.T) {
	from := QuotaWindow(time.Now())
	old := historicalAttempt("reused-sequence", from.Add(10*time.Minute), 1000)
	newAttempt := historicalAttempt("reused-sequence", from.Add(25*time.Minute), 9000)
	live := quotaSend("reused-sequence", "a", from.Add(31*time.Minute), 9000)
	store, _, now := historyFixture(t, []Row{historicalRow("old", from.Add(20*time.Minute), old), historicalRow("new-live-root", from.Add(40*time.Minute), newAttempt)}, &live)
	report := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 2)
	quotaValue(t, report.Tokens.Used, 10000)
	if report.History == nil || report.History.Attempts != 1 {
		t.Fatalf("wrong historical boundary: %+v", report.History)
	}
	live.ID = "new-v2-send"
	store.RecordQuotaAttempt(live)
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Requests.Used, 3)
}
func TestQuotaHistoryMissingUsage429AndScope(t *testing.T) {
	from := QuotaWindow(time.Now())
	missing := historicalAttempt("missing", from.Add(time.Minute), -1)
	limited := historicalAttempt("429", from.Add(2*time.Minute), -1)
	limited.StatusCode = 429
	build := limited
	build.ID = "build"
	build.Channel = "build"
	flash := limited
	flash.ID = "flash"
	flash.BaseModel = "gemini-3.8-flash"
	unsent := limited
	unsent.ID = "unsent"
	no := false
	unsent.Sent = &no
	local := historicalAttempt("local", from.Add(3*time.Minute), 900)
	local.UsageSource = "local"
	store, _, now := historyFixture(t, []Row{historicalRow("mixed", from.Add(20*time.Minute), missing, limited, build, flash, unsent, local)}, nil)
	report := quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Requests.Used, 3)
	quotaValue(t, report.Tokens.Used, 0)
	quotaValue(t, report.Accounts[0].Requests.Remaining, 0)
	quotaValue(t, report.Accounts[0].Tokens.Remaining, 0)
	quotaValue(t, report.Tokens.Remaining, 30000000)
	if report.Unreported != 3 || report.InFlight != 0 || !report.Accounts[0].Exhausted429 || report.Accounts[1].Exhausted429 {
		t.Fatalf("bad historical 429/missing handling: %+v", report)
	}
}
func TestQuotaHistoryWithoutUsableOldScopeKeepsUnknown(t *testing.T) {
	from := QuotaWindow(time.Now())
	oldWindow := historicalAttempt("prior-window", from.Add(-time.Minute), 999)
	build := historicalAttempt("build", from.Add(time.Minute), 999)
	build.Channel = "build"
	bad := historicalAttempt("missing-account", from.Add(time.Minute), 999)
	bad.AccountID = ""
	store, _, now := historyFixture(t, []Row{historicalRow("not-recoverable", from.Add(20*time.Minute), oldWindow, build, bad)}, nil)
	report := quotaRead(t, store, quotaMembers(), now)
	if report.History != nil || report.Tokens.Remaining != nil || report.Coverage != "partial" {
		t.Fatalf("invented historical coverage: %+v", report)
	}
}
func TestQuotaHistoryReplayNeverMutatesOriginalHTTPAttempts(t *testing.T) {
	from := QuotaWindow(time.Now())
	a := historicalAttempt("original", from.Add(time.Minute), 1234)
	row := historicalRow("preserve", from.Add(20*time.Minute), a)
	store, _, now := historyFixture(t, []Row{row}, nil)
	var before string
	if err := store.db.QueryRow(`SELECT attempts FROM requests WHERE id='preserve'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := importQuotaHistory(context.Background(), store.quotaConn, store.quotaSince, now); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := store.db.QueryRow(`SELECT attempts FROM requests WHERE id='preserve'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("historical HTTP record was rewritten")
	}
	quotaValue(t, quotaRead(t, store, quotaMembers(), now).Requests.Used, 1)
}
