package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func attemptTestContext() (context.Context, *accessLogMetadata) {
	metadata := &accessLogMetadata{requestID: "http-fixture-root"}
	return context.WithValue(context.Background(), accessLogContextKey{}, metadata), metadata
}

func TestAttemptLedgerLegacyUnknown(t *testing.T) {
	var legacy RequestAttempt
	if err := json.Unmarshal([]byte(`{"account":"fixture","channel":"build","error":"failed","duration_ms":7}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Kind != AttemptKindLegacy || legacy.Stage != "unknown" || legacy.Sent != nil || legacy.ResponseReceived != nil || legacy.Usage != nil || legacy.UsageSource != UsageSourceUnknown {
		t.Fatalf("legacy invented evidence: %+v", legacy)
	}
	ctx, metadata := attemptTestContext()
	AddAccessLogAttempt(ctx, RequestAttempt{Account: "fixture", Error: "outer failure"})
	snapshot := metadata.snapshot()
	if snapshot.ledger.AttemptCount != nil || snapshot.ledger.SentAttemptCount != nil || len(snapshot.attempts) != 1 || snapshot.attempts[0].Sent != nil {
		t.Fatalf("outer failure counted as physical: %+v", snapshot)
	}
}

func TestAttemptLedgerStagesAndUsage(t *testing.T) {
	ctx, metadata := attemptTestContext()
	started := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	fixtures := []aistudio.GenerationAttempt{
		{ID: "prepare", Stage: "preparation_failed", Error: "fixture validation failed"},
		{ID: "send-unknown", Stage: "sending", Sent: true},
		{ID: "response", Stage: "response_received", Sent: true, ResponseReceived: true, StatusCode: 401},
		{ID: "success", Stage: "completed", Sent: true, ResponseReceived: true, StatusCode: 200, Usage: &aistudio.Usage{}, UsageSource: "upstream"},
		{ID: "failed", Stage: "failed", Sent: true, ResponseReceived: true, StatusCode: 429, Error: "fixture quota failure"},
		{ID: "replay", Stage: "completed", Sent: true, ResponseReceived: true, Reason: "auth_replay", Usage: &aistudio.Usage{TotalTokens: 7}},
		{ID: "fallback", Stage: "completed", Sent: true, ResponseReceived: true, Reason: "build_mode_fallback", Mode: "stream", Usage: &aistudio.Usage{TotalTokens: 8}, UsageSource: "estimated"},
		{ID: "continuation", Stage: "completed", Sent: true, ResponseReceived: true, Reason: "continuation", ContinuationRound: 1},
		{ID: "unknown"},
	}
	for i := range fixtures {
		fixtures[i].Revision = 1
		fixtures[i].RequestID = "fixture-choice-0"
		fixtures[i].StartedAt = started
		fixtures[i].RequestedModel = "build/anti-truncation/fixture-model"
		fixtures[i].BaseModel = "fixture-model"
		fixtures[i].RequestedChannel = aistudio.ChannelBuild
		fixtures[i].Channel = aistudio.ChannelBuild
		fixtures[i].AntiTruncation = true
		fixtures[i].AccountID = "fixture-account-id"
		fixtures[i].Method = "FixtureGenerate"
		AddAccessLogGenerationAttempt(ctx, fixtures[i], "fixture-account")
	}
	snapshot := metadata.snapshot()
	if *snapshot.ledger.AttemptCount != 9 || *snapshot.ledger.SentAttemptCount != 7 {
		t.Fatalf("counts=%+v", snapshot.ledger)
	}
	for i, attempt := range snapshot.attempts {
		if attempt.Kind != AttemptKindGeneration || attempt.ParentID != "http-fixture-root" || attempt.RequestID != "fixture-choice-0" || attempt.ID != fixtures[i].ID || attempt.StartedAt == nil || !attempt.StartedAt.Equal(started) {
			t.Fatalf("identity lost: %+v", attempt)
		}
		if *attempt.RequestedChannel != "build" || !*attempt.AntiTruncation || attempt.BaseModel != "fixture-model" || attempt.AccountID != "fixture-account-id" || attempt.Method != "FixtureGenerate" {
			t.Fatalf("selection lost: %+v", attempt)
		}
	}
	if *snapshot.attempts[0].Sent || snapshot.attempts[0].Usage != nil || snapshot.attempts[0].UsageSource != "unknown" {
		t.Fatal("preparation must be explicitly unsent, usage unknown")
	}
	if snapshot.attempts[3].Usage == nil || snapshot.attempts[3].Usage.TotalTokens != 0 || snapshot.attempts[3].UsageSource != "upstream" {
		t.Fatal("known zero must remain distinguishable from unknown")
	}
	if snapshot.attempts[5].UsageSource != "unattributed" || snapshot.attempts[6].UsageSource != "estimated" || snapshot.attempts[7].ContinuationRound != 1 || snapshot.attempts[8].Stage != "unknown" {
		t.Fatalf("provenance/round/stage lost: %+v", snapshot.attempts)
	}
}

func TestAttemptLedgerRevisionAndEvidence(t *testing.T) {
	ctx, metadata := attemptTestContext()
	attempt := aistudio.GenerationAttempt{ID: "physical-1", Revision: 2, RequestID: "choice-0", Sent: true, ResponseReceived: true,
		Stage: "response_received", StatusCode: 200, Usage: &aistudio.Usage{TotalTokens: 17}, UsageSource: "upstream"}
	AddAccessLogGenerationAttempt(ctx, attempt, "fixture")
	attempt.Usage.TotalTokens = 999 // observer must not retain this pointer
	attempt.Revision, attempt.Stage, attempt.Usage = 3, "cancelled", nil
	attempt.Sent, attempt.ResponseReceived, attempt.StatusCode = false, false, 0
	attempt.Error = "fixture cancellation"
	AddAccessLogGenerationAttempt(ctx, attempt, "fixture")
	for _, revision := range []uint64{1, 2, 3} {
		attempt.Revision, attempt.Stage, attempt.Usage = revision, "stale", &aistudio.Usage{}
		AddAccessLogGenerationAttempt(ctx, attempt, "stale")
	}
	// A colliding ID from another choice/parent must not replace this attempt.
	attempt.Revision, attempt.RequestID = 99, "choice-1"
	AddAccessLogGenerationAttempt(ctx, attempt, "wrong-choice")
	attempt.RequestID, attempt.ParentID = "choice-0", "wrong-parent"
	AddAccessLogGenerationAttempt(ctx, attempt, "wrong-parent")
	snapshot := metadata.snapshot()
	got := snapshot.attempts[0]
	if len(snapshot.attempts) != 1 || *snapshot.ledger.AttemptCount != 1 || *snapshot.ledger.SentAttemptCount != 1 || got.Revision != 3 || got.Stage != "cancelled" || got.Account != "fixture" {
		t.Fatalf("revision/count/identity: %+v", got)
	}
	if !*got.Sent || !*got.ResponseReceived || got.StatusCode != 200 || got.Usage == nil || got.Usage.TotalTokens != 17 || got.UsageSource != "upstream" {
		t.Fatalf("observed evidence erased: %+v", got)
	}
	got.Usage.TotalTokens = 42
	*got.Sent = false
	if later := metadata.snapshot().attempts[0]; later.Usage.TotalTokens != 17 || !*later.Sent {
		t.Fatalf("snapshot aliases live metadata: %+v", later)
	}
}

func TestAttemptLedgerConcurrentChoicesAndStableParent(t *testing.T) {
	ctx, metadata := attemptTestContext()
	selection := aistudio.ModelSelection{RequestedModel: "playground/anti-truncation/fixture-alias", BaseModel: "fixture-base", Channel: aistudio.ChannelPlayground, AntiTruncation: true}
	var wait sync.WaitGroup
	for choice := range 2 {
		wait.Go(func() {
			id := fmt.Sprintf("fixture-choice-%d", choice)
			for revision := uint64(1); revision <= 100; revision++ {
				SetAccessLogModelSelection(ctx, selection)
				SetAccessLogGenerationInput(ctx, aistudio.GenerateRequest{ID: id})
				SetAccessLogTarget(ctx, selection.RequestedModel, "fixture")
				AddAccessLogGenerationAttempt(ctx, aistudio.GenerationAttempt{ID: "attempt-" + id, Revision: revision, RequestID: id, Stage: "completed", Sent: true}, "fixture")
				_ = metadata.snapshot()
			}
		})
	}
	wait.Wait()
	snapshot := metadata.snapshot()
	if snapshot.requestID != "http-fixture-root" || metadata.id() != snapshot.requestID || snapshot.model != "fixture-base" || len(snapshot.attempts) != 2 || *snapshot.ledger.AttemptCount != 2 {
		t.Fatalf("parent/base/choice isolation: %+v", snapshot)
	}
	for _, attempt := range snapshot.attempts {
		if attempt.ParentID != snapshot.requestID || attempt.Revision != 100 || attempt.RequestID == snapshot.requestID {
			t.Fatalf("choice identity lost: %+v", attempt)
		}
	}
	if snapshot.ledger.RequestedModel != selection.RequestedModel || *snapshot.ledger.RequestedChannel != "playground" || !*snapshot.ledger.AntiTruncation {
		t.Fatalf("selection=%+v", snapshot.ledger)
	}
}

func TestAttemptLedgerLogicalUsageUnknownAndUnattributed(t *testing.T) {
	ctx, metadata := attemptTestContext()
	if got := metadata.snapshot().ledger; got.UsageKnown == nil || *got.UsageKnown || got.UsageSource != "unknown" {
		t.Fatalf("unknown logical usage=%+v", got)
	}
	SetAccessLogGenerationResult(ctx, &aistudio.Usage{}, 0)
	SetAccessLogGenerationResult(ctx, nil, 0)
	if got := metadata.snapshot().ledger; !*got.UsageKnown || got.UsageSource != "unattributed" {
		t.Fatalf("unlabelled final usage must not claim upstream: %+v", got)
	}
	SetAccessLogModelSelection(ctx, aistudio.ModelSelection{RequestedModel: "fixture-base", BaseModel: "fixture-base"})
	if got := metadata.snapshot().ledger; got.RequestedChannel == nil || *got.RequestedChannel != "" || got.AntiTruncation == nil || *got.AntiTruncation {
		t.Fatalf("known auto must not look like legacy unknown: %+v", got)
	}
}

type attemptTestAdmin struct {
	AdminService
	entries []AccessLog
}

func (admin *attemptTestAdmin) RecordAccessStart(AccessLog) {}
func (admin *attemptTestAdmin) RecordAccessLog(entry AccessLog) {
	admin.entries = append(admin.entries, entry)
}

func TestAttemptLedgerMiddlewareOneLogicalRequest(t *testing.T) {
	admin := &attemptTestAdmin{}
	handler := requestLoggingMiddleware(admin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, id := range []string{"fixture-choice-0", "fixture-choice-1"} {
			SetAccessLogGenerationInput(r.Context(), aistudio.GenerateRequest{ID: id})
			AddAccessLogGenerationAttempt(r.Context(), aistudio.GenerationAttempt{ID: "physical-" + id, Revision: 1, RequestID: id, Stage: "completed", Sent: true}, "fixture")
		}
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/fixture", nil))
	if len(admin.entries) != 1 || len(admin.entries[0].Attempts) != 2 {
		t.Fatalf("logical HTTP count: %+v", admin.entries)
	}
	entry := admin.entries[0]
	for _, attempt := range entry.Attempts {
		if attempt.ParentID != entry.RequestID || attempt.RequestID == entry.RequestID {
			t.Fatalf("middleware root overwritten: %+v", entry)
		}
	}
}
