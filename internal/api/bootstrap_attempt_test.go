package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestBootstrapAttemptMetadataPreservesActualMode(t *testing.T) {
	ctx, metadata := attemptTestContext()
	value := aistudio.GenerationAttempt{ID: "fixture", RequestID: "choice", Revision: 1, Stage: "preparing", BootstrapMode: aistudio.BootstrapRestrictedFallback}
	AddAccessLogGenerationAttempt(ctx, value, "fixture-account")
	value.Revision, value.Stage, value.BootstrapMode = 2, "cancelled", ""
	AddAccessLogGenerationAttempt(ctx, value, "fixture-account")
	value.Revision, value.BootstrapMode = 3, aistudio.BootstrapLegacy
	AddAccessLogGenerationAttempt(ctx, value, "fixture-account")
	snapshot := metadata.snapshot()
	if len(snapshot.attempts) != 1 || snapshot.attempts[0].BootstrapMode != aistudio.BootstrapRestrictedFallback || *snapshot.ledger.AttemptCount != 1 || *snapshot.ledger.SentAttemptCount != 0 {
		t.Fatalf("metadata/count changed: %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot.attempts[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded RequestAttempt
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BootstrapMode != aistudio.BootstrapRestrictedFallback {
		t.Fatal("JSON metadata roundtrip lost mode")
	}
	value.ID, value.RequestID, value.Revision, value.BootstrapMode = "replacement-attempt", "choice", 1, aistudio.BootstrapLegacy
	AddAccessLogGenerationAttempt(ctx, value, "fixture-account")
	if metadata.snapshot().attempts[1].BootstrapMode != aistudio.BootstrapLegacy {
		t.Fatal("new attempt inherited previous worker mode")
	}
}

func TestBootstrapAttemptOldRowsStayUnknown(t *testing.T) {
	for _, data := range []string{`{"account":"fixture"}`, `{"kind":"generation","id":"old","stage":"completed"}`} {
		var value RequestAttempt
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			t.Fatal(err)
		}
		if value.BootstrapMode != "" {
			t.Fatal("old row invented bootstrap evidence")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "bootstrap_mode") {
			t.Fatal("old row backfilled")
		}
	}
}
