package requestdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// Frozen pre-ledger schema, independent of the current schema constant.
const legacyRequestsFixture = `CREATE TABLE requests (
 id TEXT PRIMARY KEY, time INTEGER NOT NULL, protocol TEXT NOT NULL, path TEXT NOT NULL,
 model TEXT NOT NULL, account TEXT NOT NULL, channel TEXT NOT NULL, status INTEGER NOT NULL, state TEXT NOT NULL,
 duration_ms INTEGER NOT NULL, first_event_ms INTEGER NOT NULL, queue_ms INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL, reply_tokens INTEGER NOT NULL,
 total_tokens INTEGER NOT NULL, tool_calls INTEGER NOT NULL, error TEXT NOT NULL, attempts TEXT NOT NULL);`
const legacyRollupFixture = `
 start INTEGER NOT NULL, model TEXT NOT NULL, account TEXT NOT NULL, channel TEXT NOT NULL, protocol TEXT NOT NULL,
 state TEXT NOT NULL, status INTEGER NOT NULL, requests INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL, reply_tokens INTEGER NOT NULL, total_tokens INTEGER NOT NULL,
 duration_ms INTEGER NOT NULL, first_event_ms INTEGER NOT NULL, first_events INTEGER NOT NULL, queue_ms INTEGER NOT NULL,
 duration_hist BLOB NOT NULL, first_event_hist BLOB NOT NULL, last_time INTEGER NOT NULL,
 PRIMARY KEY (start, model, account, channel, protocol, state, status)`
const legacyAttemptsFixture = `[{"account":"fixture-old","channel":"build","error":"fixture failure","duration_ms":9}]`

func ledgerPtr[T any](value T) *T { return &value }

func makeLegacyLedgerFixture(t *testing.T, path string, at time.Time, cached bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacyRequestsFixture + `CREATE TABLE hours (` + legacyRollupFixture + `) WITHOUT ROWID; CREATE TABLE days (` + legacyRollupFixture + `) WITHOUT ROWID;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO requests VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"legacy", at.UnixMilli(), "openai-chat", "/v1/chat/completions", "fixture-base", "fixture-old", "build", 200, "completed", 100, 0, 0,
		3, 0, 7, 10, 0, "", legacyAttemptsFixture); err != nil {
		t.Fatal(err)
	}
	day, _ := localDay(at.UnixMilli())
	for table, start := range map[string]int64{"hours": at.UnixMilli(), "days": day} {
		if _, err := db.Exec(`INSERT INTO `+table+` VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			start, "fixture-base", "fixture-old", "build", "openai-chat", "completed", 200, 1, 3, 0, 7, 10, 100, 0, 0, 0,
			single(100).encode(), counts{}.encode(), at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if cached {
		for _, table := range []string{"requests", "hours", "days"} {
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE ` + table + ` SET cached_tokens=2`); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAttemptLedgerIncrementalMigration(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached_column_%v", cached), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.db")
			at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
			makeLegacyLedgerFixture(t, path, at, cached)
			for reopen := range 2 { // migration must be idempotent
				store, err := Open(path, func(level, message string) { t.Errorf("%s %s", level, message) })
				if err != nil {
					t.Fatal(err)
				}
				page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Second), To: at.Add(time.Minute), Limit: 10})
				if err != nil || len(page.Items) != 1 {
					t.Fatalf("records=%+v err=%v", page, err)
				}
				row := page.Items[0]
				if row.ID != "legacy" || row.TotalTokens != 10 || row.UsageKnown != nil || row.UsageSource != "unknown" || row.RequestedChannel != nil || row.AntiTruncation != nil || row.AttemptCount != nil || row.SentAttemptCount != nil || row.BaseModel != "" {
					t.Fatalf("history rewritten/inferred: %+v", row)
				}
				if row.Attempts[0].Kind != api.AttemptKindLegacy || row.Attempts[0].Sent != nil || row.Attempts[0].ResponseReceived != nil || row.Attempts[0].Stage != "unknown" || row.Attempts[0].Usage != nil {
					t.Fatalf("legacy attempt became physical: %+v", row.Attempts[0])
				}
				var raw string
				var unknown bool
				if err := store.db.QueryRow(`SELECT r.attempts, m.id IS NULL FROM requests r LEFT JOIN request_metadata m ON m.id=r.id WHERE r.id='legacy'`).Scan(&raw, &unknown); err != nil || raw != legacyAttemptsFixture || !unknown {
					t.Fatalf("migration changed old row: raw=%q null=%v err=%v", raw, unknown, err)
				}
				var columns int
				if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests')`).Scan(&columns); err != nil || columns != 20 {
					t.Fatalf("release requests layout changed: columns=%d err=%v", columns, err)
				}
				wantCached := int64(0)
				if cached {
					wantCached = 2
				}
				for _, table := range []string{"requests", "hours", "days"} {
					var tokens int64
					if err := store.db.QueryRow(`SELECT cached_tokens FROM ` + table).Scan(&tokens); err != nil || tokens != wantCached {
						t.Fatalf("%s cached=%d err=%v", table, tokens, err)
					}
				}
				// Both raw-record and hour-rollup plans keep the old logical totals.
				for _, span := range []time.Duration{time.Minute, time.Hour} {
					report, err := store.Usage(context.Background(), api.UsageQuery{From: at, To: at.Add(span), Bucket: time.Minute, Location: time.UTC, Stack: "model"})
					if err != nil || report.Totals.Requests != 1 || report.Totals.TotalTokens != 10 || report.Totals.CachedTokens != wantCached {
						t.Fatalf("reopen=%d report=%+v err=%v", reopen, report.Totals, err)
					}
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAttemptLedgerRecordsAPIExportAndBaseRollup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	makeLegacyLedgerFixture(t, path, at, true)
	logs := func(level, message string) { t.Errorf("%s %s", level, message) }
	store, err := Open(path, logs)
	if err != nil {
		t.Fatal(err)
	}
	attempts := []api.RequestAttempt{
		{Kind: api.AttemptKindGeneration, ID: "prepare", Revision: 1, RequestID: "choice-0", ParentID: "strict", Stage: "preparation_failed", Sent: ledgerPtr(false), ResponseReceived: ledgerPtr(false), UsageSource: "unknown"},
		{Kind: api.AttemptKindGeneration, ID: "success", Revision: 3, RequestID: "choice-1", ParentID: "strict", Stage: "completed", Sent: ledgerPtr(true), ResponseReceived: ledgerPtr(true), Usage: &aistudio.Usage{}, UsageSource: "upstream"},
	}
	rows := []Row{
		{ID: "strict", Time: at.Add(time.Minute), Model: "build/anti-truncation/fixture-alias", State: "completed", Status: 200, Channel: "build", Attempts: attempts,
			RequestLedgerMetadata: api.RequestLedgerMetadata{RequestedModel: "build/anti-truncation/fixture-alias", BaseModel: "fixture-base", RequestedChannel: ledgerPtr("build"), AntiTruncation: ledgerPtr(true), UsageKnown: ledgerPtr(false), UsageSource: "unknown", AttemptCount: ledgerPtr(2), SentAttemptCount: ledgerPtr(1)}},
		{ID: "known-zero", Time: at.Add(2 * time.Minute), Model: "playground/fixture-base", State: "completed", Status: 200, Channel: "playground",
			RequestLedgerMetadata: api.RequestLedgerMetadata{RequestedModel: "playground/fixture-base", BaseModel: "fixture-base", RequestedChannel: ledgerPtr("playground"), AntiTruncation: ledgerPtr(false), UsageKnown: ledgerPtr(true), UsageSource: "estimated"}},
		{ID: "unattributed", Time: at.Add(3 * time.Minute), Model: "fixture-base", State: "completed", Status: 200, TotalTokens: 5,
			RequestLedgerMetadata: api.RequestLedgerMetadata{RequestedModel: "fixture-base", BaseModel: "fixture-base", RequestedChannel: ledgerPtr(""), AntiTruncation: ledgerPtr(false), UsageKnown: ledgerPtr(true)}},
	}
	for _, row := range rows {
		store.Record(row)
	}
	store.Record(rows[0]) // one HTTP ID still counts once
	attempts[1].Usage.TotalTokens = 999
	*rows[0].AttemptCount = 999
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logs)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	query := api.UsageRecordQuery{From: at, To: at.Add(time.Hour), Limit: 10, Filters: api.UsageFilters{"model": {"fixture-base"}}}
	page, err := store.Records(context.Background(), query)
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	byID := map[string]api.UsageRecord{}
	for _, item := range page.Items {
		byID[item.ID] = item
	}
	strict := byID["strict"]
	if strict.Model != "fixture-base" || strict.RequestedModel != rows[0].RequestedModel || strict.Channel != "build" || *strict.RequestedChannel != "build" || !*strict.AntiTruncation || *strict.UsageKnown || *strict.AttemptCount != 2 || *strict.SentAttemptCount != 1 {
		t.Fatalf("strict detail=%+v", strict)
	}
	if strict.Attempts[0].Usage != nil || strict.Attempts[1].Usage == nil || strict.Attempts[1].Usage.TotalTokens != 0 || strict.Attempts[1].Revision != 3 || strict.Attempts[1].ParentID != "strict" {
		t.Fatalf("attempt evidence=%+v", strict.Attempts)
	}
	if !*byID["known-zero"].UsageKnown || byID["known-zero"].UsageSource != "estimated" || byID["unattributed"].UsageSource != "unattributed" || *byID["unattributed"].RequestedChannel != "" {
		t.Fatalf("usage provenance=%+v", byID)
	}
	report, err := store.Usage(context.Background(), api.UsageQuery{From: at, To: at.Add(time.Hour), Bucket: time.Minute, Location: time.UTC, Stack: "model"})
	if err != nil || report.Totals.Requests != 4 || report.Totals.TotalTokens != 15 || len(report.Groups["model"]) != 1 || report.Groups["model"][0].Key != "fixture-base" {
		t.Fatalf("logical/base totals=%+v models=%+v err=%v", report.Totals, report.Groups["model"], err)
	}
	// Existing status/search/pagination entrypoints remain compatible.
	filtered := query
	filtered.Search, filtered.Status = "strict", 200
	if found, err := store.Records(context.Background(), filtered); err != nil || len(found.Items) != 1 || found.Items[0].ID != "strict" {
		t.Fatalf("filter=%+v err=%v", found, err)
	}
	paged := query
	paged.Limit = 1
	first, err := store.Records(context.Background(), paged)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("pagination=%+v err=%v", first, err)
	}
	paged.Before = &api.UsageCursor{Time: first.Items[0].Time, ID: first.Items[0].ID}
	if next, err := store.Records(context.Background(), paged); err != nil || len(next.Items) != 1 || next.Items[0].ID == first.Items[0].ID {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	// Exercise the real record and CSV handlers without opening a socket.
	handler := api.NewHandler(nil, api.Config{Ledger: store})
	params := url.Values{"from": {at.Format(time.RFC3339)}, "to": {at.Add(time.Hour).Format(time.RFC3339)}, "model": {"fixture-base"}}
	for _, endpoint := range []string{"/api/usage/records", "/api/usage/records.csv"} {
		request := httptest.NewRequest(http.MethodGet, "http://localhost"+endpoint+"?"+params.Encode(), nil)
		request.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", endpoint, response.Code, response.Body)
		}
		if strings.HasSuffix(endpoint, ".csv") {
			checkAttemptCSV(t, response.Body.Bytes())
		} else {
			var result api.UsageRecordPage
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Items) != 4 || result.Items[2].RequestedModel != strict.RequestedModel || result.Items[2].Attempts[1].Usage == nil {
				t.Fatalf("HTTP records=%+v err=%v", result, err)
			}
		}
	}
}

func newLedgerFixtureRow(id string, at time.Time) Row {
	return Row{
		ID: id, Time: at, Protocol: "openai-chat", Path: "/v1/chat/completions", Model: "build/anti-truncation/fixture-alias",
		Account: "fixture-new", Channel: "build", Status: 200, State: "completed", TotalTokens: 13,
		RequestLedgerMetadata: api.RequestLedgerMetadata{
			RequestedModel: "build/anti-truncation/fixture-alias", BaseModel: "fixture-base", RequestedChannel: ledgerPtr("build"),
			AntiTruncation: ledgerPtr(true), UsageKnown: ledgerPtr(true), UsageSource: "mixed", AttemptCount: ledgerPtr(1), SentAttemptCount: ledgerPtr(1),
		},
		Attempts: []api.RequestAttempt{{ID: id + ":physical", Kind: api.AttemptKindGeneration, Revision: 2, RequestID: "fixture-choice", ParentID: id,
			Stage: "completed", Sent: ledgerPtr(true), ResponseReceived: ledgerPtr(true), Usage: &aistudio.Usage{TotalTokens: 3}, UsageSource: "upstream"}},
	}
}

func TestAttemptLedgerReleaseRollbackRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	makeLegacyLedgerFixture(t, path, at, true)
	logger := func(level, message string) { t.Errorf("%s %s", level, message) }
	store, err := Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	newRow := newLedgerFixtureRow("new-before-rollback", at.Add(time.Minute))
	store.Record(newRow)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// No executable, backups, table rebuild or schema downgrade: simulate only
	// the release's exact positional 20-column write on the SAME migrated DB.
	oldDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer oldDB.Close()
	tx, err := oldDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	oldRow := Row{ID: "old-after-rollback", Time: at.Add(2 * time.Minute), Protocol: "openai-chat", Path: "/v1/chat/completions",
		Model: "fixture-base", Account: "fixture-old", Channel: "build", Status: 200, State: "completed", InputTokens: 1, ReplyTokens: 4, TotalTokens: 5}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO requests VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		oldRow.ID, oldRow.Time.UnixMilli(), oldRow.Protocol, oldRow.Path, oldRow.Model, oldRow.Account, oldRow.Channel, oldRow.Status, oldRow.State,
		int64(0), int64(0), int64(0), oldRow.InputTokens, oldRow.ReasoningTokens, oldRow.ReplyTokens, oldRow.TotalTokens, 0, "", legacyAttemptsFixture, int64(0)); err != nil {
		t.Fatalf("release 20-column INSERT failed after new Open/write: %v", err)
	}
	// The existing rollup helper is unchanged from the baseline; an old writer
	// also updates these unchanged tables. It never writes request_metadata.
	value := rowEntry(&oldRow)
	day, _ := localDay(value.time)
	for table, start := range map[string]int64{"hours": floorDiv(value.time, hourMS) * hourMS, "days": day} {
		if err := mergeRollup(tx, table, keyOf(start, value), value.clone()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Make an expired new-format row, then let the simulated old release prune
	// requests only, leaving the side-table orphan a new Open must clean.
	fixtureStore := &Store{db: oldDB, log: logger}
	expired := newLedgerFixtureRow("pruned-by-old", time.Now().Add(-91*24*time.Hour))
	if err := fixtureStore.write([]write{{row: &expired}}); err != nil {
		t.Fatal(err)
	}
	result, err := oldDB.Exec(`DELETE FROM requests WHERE time < ?`, time.Now().Add(-usageRetention).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if removed, _ := result.RowsAffected(); removed != 1 {
		t.Fatalf("old prune removed %d requests, want expired fixture only", removed)
	}
	var orphan int
	if err := oldDB.QueryRow(`SELECT COUNT(*) FROM request_metadata WHERE id='pruned-by-old'`).Scan(&orphan); err != nil || orphan != 1 {
		t.Fatalf("missing orphan fixture: %d %v", orphan, err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Replayed request IDs must not replace new evidence or backfill an old
	// release row with information that did not accompany its original write.
	for _, id := range []string{newRow.ID, oldRow.ID} {
		duplicate := newLedgerFixtureRow(id, at.Add(3*time.Minute))
		duplicate.RequestedModel, duplicate.UsageSource = "wrong-duplicate", "estimated"
		if err := store.write([]write{{row: &duplicate}}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at, To: at.Add(time.Hour), Limit: 10})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("rollback lost/duplicated requests: %+v err=%v", page, err)
	}
	for _, row := range page.Items {
		if row.ID == newRow.ID {
			if row.RequestedModel != newRow.RequestedModel || row.BaseModel != "fixture-base" || row.UsageSource != "mixed" || *row.AttemptCount != 1 || row.Attempts[0].ID != newRow.Attempts[0].ID || row.Attempts[0].UsageSource != "upstream" {
				t.Fatalf("new metadata lost on rollback/recovery: %+v", row)
			}
		} else if row.UsageKnown != nil || row.UsageSource != "unknown" || row.RequestedChannel != nil || row.AttemptCount != nil || row.Attempts[0].Kind != api.AttemptKindLegacy || row.Attempts[0].Sent != nil {
			t.Fatalf("old write fabricated new evidence: %+v", row)
		}
	}
	var metadataCount, requestCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM request_metadata`).Scan(&metadataCount); err != nil || metadataCount != 1 {
		t.Fatalf("orphan cleanup/backfill: metadata=%d err=%v", metadataCount, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&requestCount); err != nil || requestCount != 3 {
		t.Fatalf("recovery deleted a live request: requests=%d err=%v", requestCount, err)
	}
	for _, span := range []time.Duration{10 * time.Minute, time.Hour} {
		report, err := store.Usage(context.Background(), api.UsageQuery{From: at, To: at.Add(span), Bucket: time.Minute, Location: time.UTC, Stack: "model"})
		if err != nil || report.Totals.Requests != 3 || report.Totals.TotalTokens != 28 {
			t.Fatalf("rollback logical totals=%+v err=%v", report.Totals, err)
		}
	}
}

func TestAttemptLedgerMetadataTransactionAtomicity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fixture.db"), func(level, message string) { t.Errorf("%s %s", level, message) })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TRIGGER fixture_fail_metadata BEFORE INSERT ON request_metadata BEGIN SELECT RAISE(ABORT,'fixture metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	row := newLedgerFixtureRow("atomic", time.Now().UTC())
	if err := store.write([]write{{row: &row}}); err == nil {
		t.Fatal("fixture metadata write should fail")
	}
	for _, table := range []string{"requests", "request_metadata", "hours", "days"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial transaction in %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := store.db.Exec(`DROP TRIGGER fixture_fail_metadata`); err != nil {
		t.Fatal(err)
	}
	if err := store.write([]write{{row: &row}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"requests", "request_metadata", "hours", "days"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing committed row in %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestAttemptLedgerPruneKeepsRecentRequests(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fixture.db"), func(level, message string) { t.Errorf("%s %s", level, message) })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recent := newLedgerFixtureRow("recent", time.Now().UTC())
	expired := newLedgerFixtureRow("expired", time.Now().Add(-91*24*time.Hour))
	if err := store.write([]write{{row: &recent}, {row: &expired}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO request_metadata(id) VALUES ('orphan')`); err != nil {
		t.Fatal(err)
	}
	store.prune()
	for _, table := range []string{"requests", "request_metadata"} {
		var ids string
		if err := store.db.QueryRow(`SELECT GROUP_CONCAT(id) FROM ` + table).Scan(&ids); err != nil || ids != "recent" {
			t.Fatalf("prune touched fresh request or left orphan: %s ids=%s err=%v", table, ids, err)
		}
	}
}

func checkAttemptCSV(t *testing.T, data []byte) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		t.Fatal("missing existing UTF-8 BOM")
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))).ReadAll()
	if err != nil || len(records) != 5 {
		t.Fatalf("CSV rows=%d err=%v", len(records), err)
	}
	oldHeader := []string{"time", "id", "protocol", "path", "model", "account", "channel", "status", "state", "duration_ms", "first_event_ms", "queue_ms", "input_tokens", "cached_tokens", "reasoning_tokens", "reply_tokens", "total_tokens", "tool_calls", "error", "attempts"}
	if len(records[0]) != 28 || !reflect.DeepEqual(records[0][:20], oldHeader) {
		t.Fatalf("legacy CSV columns changed: %v", records[0])
	}
	for _, row := range records[1:] {
		if row[1] == "unattributed" && row[22] != "auto" {
			t.Fatalf("CSV lost known auto policy: %v", row)
		}
		if row[1] == "legacy" && (row[22] != "" || row[23] != "" || row[24] != "" || row[25] != "unknown" || row[26] != "") {
			t.Fatalf("CSV invented legacy evidence: %v", row)
		}
		if row[1] == "strict" && (row[20] != "build/anti-truncation/fixture-alias" || row[24] != "false" || row[26] != "2" || row[27] != "1") {
			t.Fatalf("CSV lost new detail: %v", row)
		}
	}
}
