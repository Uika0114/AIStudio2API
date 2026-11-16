package requestdb

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func TestBootstrapAttemptPersistenceDoesNotChangeLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	at := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	store, err := Open(path, func(level, message string) { t.Errorf("%s %s", level, message) })
	if err != nil {
		t.Fatal(err)
	}
	row := newLedgerFixtureRow("bootstrap-mode", at)
	row.Attempts[0].BootstrapMode = aistudio.BootstrapRestrictedFallback
	store.Record(row)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var raw string
	if err := store.db.QueryRow(`SELECT attempts FROM requests WHERE id=?`, row.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var attempts []api.RequestAttempt
	if err := json.Unmarshal([]byte(raw), &attempts); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].BootstrapMode != aistudio.BootstrapRestrictedFallback {
		t.Fatal("attempt mode lost across write/reopen")
	}
	var columns int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests')`).Scan(&columns); err != nil || columns != 20 {
		t.Fatalf("main schema changed: columns=%d err=%v", columns, err)
	}
	page, err := store.Records(context.Background(), api.UsageRecordQuery{From: at.Add(-time.Minute), To: at.Add(time.Minute), Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Attempts[0].BootstrapMode != aistudio.BootstrapRestrictedFallback {
		t.Fatalf("records roundtrip: %+v %v", page, err)
	}
	report, err := store.Usage(context.Background(), api.UsageQuery{From: at.Add(-time.Minute), To: at.Add(time.Minute), Bucket: time.Minute, Location: time.UTC, Stack: "model"})
	if err != nil || report.Totals.Requests != 1 || report.Totals.TotalTokens != row.TotalTokens {
		t.Fatalf("ledger changed: %+v %v", report.Totals, err)
	}
}
