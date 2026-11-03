package requestdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func ledgerOutputTiming() *aistudio.OutputTiming {
	return &aistudio.OutputTiming{
		Version: aistudio.OutputTimingVersion, Method: aistudio.OutputTimingMethod,
		AttemptID: "fixture/physical", RequestID: "fixture/request", LifecycleID: "fixture/lifetime", Purpose: "reply",
		Status: aistudio.OutputTimingMeasured, Complete: true, Terminal: true, HasOutput: true,
		GeneratedTokens: 520, CountSource: "output_plus_reasoning", Counts: aistudio.GenerationCounts{Output: 420, Reasoning: 100, OutputKnown: true, ReasoningKnown: true}, TokenSource: "upstream", DurationMS: 8000,
	}
}

func TestOutputTimingMigrationCloneColdReadDuplicateAndLegacyWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	at := time.Now().UTC().Truncate(time.Second)
	makeLegacyLedgerFixture(t, path, at.Add(-time.Minute), true)
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
	row := newLedgerFixtureRow("timed", at)
	row.OutputTiming = ledgerOutputTiming()
	store.Record(row)
	row.OutputTiming.DurationMS = 1
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	duplicate := newLedgerFixtureRow("timed", at)
	duplicate.OutputTiming = ledgerOutputTiming()
	duplicate.OutputTiming.DurationMS = 1
	if err = store.write([]write{{row: &duplicate}}); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour), Limit: 10})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("cold read: %+v %v", page, err)
	}
	got := page.Items[0]
	if got.OutputTPS == nil || *got.OutputTPS != 65 || got.OutputTiming.DurationMS != 8000 || page.Items[1].OutputTPS != nil || page.Items[1].OutputTiming != nil {
		t.Fatalf("clone/duplicate/history changed evidence: %+v", page)
	}
	encoded, err := json.Marshal(got)
	if err != nil || bytes.Contains(encoded, []byte(`"output_timing"`)) || !bytes.Contains(encoded, []byte(`"output_tps":65`)) {
		t.Fatalf("public record should expose only numeric TPS: %s %v", encoded, err)
	}
	var count int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests')`).Scan(&count); err != nil || count != 20 {
		t.Fatalf("requests layout %d %v", count, err)
	}
	// Simulate the unchanged old executable's positional writer on this migrated DB.
	if _, err = store.db.Exec(`INSERT INTO requests VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"old-writer", at.Add(time.Second).UnixMilli(), "openai-chat", "/v1/chat/completions", "fixture-model", "fixture-account", "playground", 200, "completed", 100, 0, 0, 3, 0, 7, 10, 0, "", "[]", 0); err != nil {
		t.Fatal(err)
	}
	page, err = store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour), Limit: 10})
	if err != nil || len(page.Items) != 3 || page.Items[0].ID != "old-writer" || page.Items[0].OutputTPS != nil {
		t.Fatalf("old writer compatibility %+v %v", page, err)
	}
	var csvOut bytes.Buffer
	if err = store.ExportRecords(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour)}, &csvOut); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(csvOut.String(), "\xef\xbb\xbf"))).ReadAll()
	if err != nil || len(rows[0]) != 29 || strings.Contains(strings.Join(rows[0], ","), "output_tps") || strings.Contains(csvOut.String(), "output_timing") {
		t.Fatalf("must not expand CSV: %v %v", rows, err)
	}
}

func TestOutputTimingInvalidOptionalEvidenceDoesNotLoseRequest(t *testing.T) {
	for name, mutate := range map[string]func(*Row){
		"missing":         func(r *Row) { r.OutputTiming = nil },
		"nonfinite":       func(r *Row) { r.OutputTiming.DurationMS = math.NaN() },
		"oversized":       func(r *Row) { r.OutputTiming.RequestID = strings.Repeat("x", outputTimingJSONLimit) },
		"unknown version": func(r *Row) { r.OutputTiming.Version++ },
		"zero window":     func(r *Row) { r.OutputTiming.DurationMS = 0 },
		"local count":     func(r *Row) { r.OutputTiming.TokenSource = "local" },
		"failed":          func(r *Row) { r.State = "failed"; r.Status = 502 },
		"cancelled":       func(r *Row) { r.State = "cancelled"; r.Status = 499 },
		"summary":         func(r *Row) { r.OutputTiming.Purpose = "compaction" },
	} {
		t.Run(name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "fixture.db"), func(level, message string) { t.Errorf("%s %s", level, message) })
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			at := time.Now().UTC()
			row := newLedgerFixtureRow("fixture", at)
			row.OutputTiming = ledgerOutputTiming()
			mutate(&row)
			if err = store.write([]write{{row: &row}}); err != nil {
				t.Fatal(err)
			}
			page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Minute), To: at.Add(time.Minute), Limit: 10})
			if err != nil || len(page.Items) != 1 || page.Items[0].OutputTPS != nil || page.Items[0].TotalTokens != row.TotalTokens {
				t.Fatalf("invalid timing lost/changed row: %+v %v", page, err)
			}
		})
	}
}

func TestOutputTimingMalformedColdJSONAndAtomicFailure(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fixture.db"), func(level, message string) { t.Errorf("%s %s", level, message) })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Now().UTC()
	row := newLedgerFixtureRow("good", at)
	row.OutputTiming = ledgerOutputTiming()
	if err = store.write([]write{{row: &row}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`UPDATE request_metadata SET output_timing='not-json' WHERE id='good'`); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Minute), To: at.Add(time.Minute), Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].OutputTPS != nil {
		t.Fatalf("malformed optional JSON broke records: %+v %v", page, err)
	}
	if _, err = store.db.Exec(`CREATE TRIGGER reject_timing_metadata BEFORE INSERT ON request_metadata WHEN NEW.id='failed-transaction' BEGIN SELECT RAISE(ABORT,'fixture metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := newLedgerFixtureRow("failed-transaction", at)
	failed.OutputTiming = ledgerOutputTiming()
	if err = store.write([]write{{row: &failed}}); err == nil {
		t.Fatal("expected real DB failure")
	}
	var count int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id='failed-transaction'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial primary row escaped transaction: %d %v", count, err)
	}
}

func TestOutputTimingLegacyV1EvidenceColdRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v1.db")
	logger := func(level, message string) { t.Errorf("%s %s", level, message) }
	store, err := Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	row := newLedgerFixtureRow("v1", at)
	row.OutputTiming = &aistudio.OutputTiming{Version: 1, Method: "upstream_non_thinking_window_v1", AttemptID: "old", LifecycleID: "old-life", Purpose: "reply", Status: "measured", Complete: true, Terminal: true, HasOutput: true, ProgressBatches: 2, StartOutputTokens: 100, EndOutputTokens: 620, TokenSource: "upstream", DurationMS: 8000}
	store.Record(row)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Minute), To: at.Add(time.Minute), Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("legacy read %v %v", page, err)
	}
	got := page.Items[0]
	if got.OutputTPS != nil || got.OutputTiming == nil || got.OutputTiming.Version != 1 || got.OutputTiming.EndOutputTokens != 620 || got.OutputTiming.ProgressBatches != 2 {
		t.Fatalf("v1 evidence lost or reinterpreted: %+v", got)
	}
	data, _ := json.Marshal(got)
	if !bytes.Contains(data, []byte(`"output_tps":null`)) {
		t.Fatalf("v1 leaked old metric: %s", data)
	}
}
