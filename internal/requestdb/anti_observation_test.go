package requestdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAntiObservationMigrationColdReadCSVAndDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	at := time.Now().UTC().Truncate(time.Second)
	makeLegacyLedgerFixture(t, path, at.Add(-time.Minute), true)
	// Simulate the existing 9-column metadata table, not the new declaration.
	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(requestMetadataSchema); err != nil {
		t.Fatal(err)
	}
	old.Close()
	logger := func(level, message string) { t.Errorf("%s %s", level, message) }
	store, err := Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	row := newLedgerFixtureRow("observed", at)
	row.State = "blocked"
	row.AntiObservations = []aistudio.AntiTruncationObservation{{Version: 1, ID: "transport", Revision: 4, RequestID: "choice", ParentID: row.ID, Purpose: "reply", Behavior: "legacy-reply-v1", Injected: true, Sent: true, Complete: true, End: "finished", Rounds: []aistudio.AntiRoundObservation{{AttemptID: "physical", Calls: 1, Legal: 1, Complete: true, FinishReason: "provider_19", End: "finished"}}}}
	store.Record(row)
	row.AntiObservations[0].Rounds[0].Calls = 999
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	duplicate := newLedgerFixtureRow("observed", at)
	duplicate.AntiObservations = []aistudio.AntiTruncationObservation{{ID: "wrong"}}
	if err = store.write([]write{{row: &duplicate}}); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour), Limit: 10})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("records: %+v %v", page, err)
	}
	got := page.Items[0]
	if len(got.AntiObservations) != 1 || got.AntiObservations[0].ID != "transport" || got.AntiObservations[0].Rounds[0].Calls != 1 || got.AntiObservations[0].Rounds[0].FinishReason != "provider_19" || page.Items[1].AntiObservations != nil {
		t.Fatalf("cold evidence: %+v", page)
	}
	var columns int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests')`).Scan(&columns); err != nil || columns != 20 {
		t.Fatalf("old positional layout: %d %v", columns, err)
	}
	var out bytes.Buffer
	if err = store.ExportRecords(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour)}, &out); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(out.String(), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var parsed []aistudio.AntiTruncationObservation
	if err = json.Unmarshal([]byte(records[1][28]), &parsed); err != nil || len(parsed) != 1 || parsed[0].Rounds[0].Calls != 1 || records[2][28] != "" {
		t.Fatalf("CSV evidence: %v %v", records, err)
	}
	if _, err = store.db.Exec(`UPDATE requests SET time=? WHERE id='observed'`, time.Now().Add(-91*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	store.prune()
	var count int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM request_metadata WHERE id='observed'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan observation retained")
	}
}

func TestAntiObservationKnownEmptyOldWriterAndMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	logger := func(l, m string) { t.Errorf("%s %s", l, m) }
	store, err := Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Now().UTC()
	empty := newLedgerFixtureRow("empty", at)
	empty.AntiObservations = []aistudio.AntiTruncationObservation{}
	store.Record(empty)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// The old metadata writer names its original columns; the new one stays NULL.
	legacy := newLedgerFixtureRow("old-writer", at.Add(time.Millisecond))
	if err = store.write([]write{{row: &legacy}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`UPDATE request_metadata SET anti_observations=NULL WHERE id='old-writer'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`INSERT OR REPLACE INTO request_metadata (id,requested_model,base_model,requested_channel,anti_truncation,usage_known,usage_source,attempt_count,sent_attempt_count) VALUES ('old-writer','fixture','fixture','playground',1,0,'unknown',1,1)`); err != nil {
		t.Fatal(err)
	}
	query := api.UsageRecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour), Limit: 10}
	page, err := store.Records(context.Background(), query)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("read=%+v %v", page, err)
	}
	if page.Items[0].AntiObservations != nil || page.Items[1].AntiObservations == nil || len(page.Items[1].AntiObservations) != 0 {
		t.Fatal("NULL and explicit empty conflated")
	}
	if _, err = store.db.Exec(`UPDATE request_metadata SET anti_observations='{bad-json' WHERE id='empty'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Records(context.Background(), query); err == nil {
		t.Fatal("bad JSON silently became no calls")
	}
}

func TestAntiObservationQueueLossAndAtomicFailure(t *testing.T) {
	// Deterministic full queue, no blocked writer or unbounded waits.
	queue := &Store{writes: make(chan write, 1)}
	row := newLedgerFixtureRow("first", time.Now())
	row.AntiObservations = []aistudio.AntiTruncationObservation{{ID: "fixture"}}
	queue.Record(row)
	queue.Record(row)
	if queue.dropped.Load() != 1 {
		t.Fatal("queue loss diagnostic missing")
	}
	var failures atomic.Int64
	path := filepath.Join(t.TempDir(), "fixture.db")
	store, err := Open(path, func(l, m string) {
		if l == "ERROR" {
			failures.Add(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`CREATE TRIGGER anti_write_failure BEFORE INSERT ON request_metadata BEGIN SELECT RAISE(ABORT,'fixture metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	store.Record(row)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if failures.Load() == 0 {
		t.Fatal("write failure not visible")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id='first'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial transaction retained: %d %v", count, err)
	}
}
