package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

// Exercises the real API middleware and app projections, without runtime,
// account access, a listener or upstream. Only Generate is a fixture.
type ledgerFixtureService struct {
	aistudio.Service
	usage *aistudio.Usage
}

func (service ledgerFixtureService) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	selection := request.Selection
	selection.BaseModel = "fixture-base"
	api.SetAccessLogModelSelection(ctx, selection)
	api.SetAccessLogGenerationInput(ctx, request)
	api.SetAccessLogGenerationConfig(ctx, request.Config)
	attempt := aistudio.GenerationAttempt{
		ID: request.ID + ":0", Revision: 1, RequestID: request.ID, Stage: "preparing", StartedAt: time.Now().UTC(),
		RequestedModel: selection.RequestedModel, BaseModel: "fixture-base", RequestedChannel: selection.Channel,
		Channel: aistudio.ChannelPlayground, AntiTruncation: true, AccountID: "fixture-account", Method: "fixture-generate", Mode: "stream",
	}
	api.AddAccessLogGenerationAttempt(ctx, attempt, "fixture-label")
	api.SetAccessLogChannel(ctx, "playground")
	api.SetAccessLogTarget(ctx, request.Model, "fixture-label") // emits the start projection
	attempt.Revision, attempt.Stage, attempt.Sent, attempt.ResponseReceived = 2, "completed", true, true
	attempt.StatusCode = 200
	attempt.Usage = &aistudio.Usage{TotalTokens: 3, Source: "upstream"}
	api.AddAccessLogGenerationAttempt(ctx, attempt, "fixture-label")
	attempt.ID, attempt.Revision, attempt.ContinuationRound, attempt.Reason = request.ID+":1", 1, 1, "continuation"
	attempt.Usage = &aistudio.Usage{TotalTokens: 5, Source: "estimated"}
	api.AddAccessLogGenerationAttempt(ctx, attempt, "fixture-label")
	// Root usage is supplied by the core, not reconstructed from attempts.
	api.SetAccessLogGenerationResult(ctx, service.usage, 0)
	api.SetAccessLogFinishReason(ctx, "stop")
	events := make(chan aistudio.Event, 2)
	if service.usage != nil {
		events <- aistudio.Event{Kind: aistudio.EventUsage, Usage: service.usage, Channel: selection.Channel}
	}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "stop", Channel: selection.Channel}
	close(events)
	return events, nil
}

type ledgerFixtureAdmin struct {
	api.AdminService
	mu        sync.Mutex
	store     *requestdb.Store
	starts    []api.AccessLog
	finals    []api.AccessLog
	startLogs []*api.RequestLog
	finalLogs []*api.RequestLog
}

func (admin *ledgerFixtureAdmin) RecordAccessStart(entry api.AccessLog) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.starts = append(admin.starts, entry)
	admin.startLogs = append(admin.startLogs, requestLogData(entry))
}

func (admin *ledgerFixtureAdmin) RecordAccessLog(entry api.AccessLog) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.finals = append(admin.finals, entry)
	data, _, _ := finishedRequestLog(entry)
	admin.finalLogs = append(admin.finalLogs, data)
	// Same authorized-POST guard and projection as runtimeManager.RecordAccessLog.
	if entry.Method == http.MethodPost && entry.Authorized {
		admin.store.Record(requestRow(entry, time.Now().UTC()))
	}
}

func TestRequestLedgerHTTPProjectionToDatabase(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		usage  *aistudio.Usage
		source string
	}{
		{"unknown", nil, "unknown"},
		{"known-zero", &aistudio.Usage{}, "unattributed"},
		{"upstream", &aistudio.Usage{TotalTokens: 13, Source: "upstream"}, "upstream"},
		{"estimated", &aistudio.Usage{TotalTokens: 13, Source: "estimated"}, "estimated"},
		{"mixed", &aistudio.Usage{TotalTokens: 13, Source: "mixed"}, "mixed"},
		{"unattributed", &aistudio.Usage{TotalTokens: 13}, "unattributed"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.db")
			logger := func(level, message string) { t.Errorf("%s %s", level, message) }
			store, err := requestdb.Open(path, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			admin := &ledgerFixtureAdmin{store: store}
			handler := api.NewHandler(ledgerFixtureService{usage: fixture.usage}, api.Config{Admin: admin})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"playground/anti-truncation/fixture-alias","messages":[{"role":"user","content":"fixture"}]}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body)
			}
			if len(admin.starts) != 1 || len(admin.finals) != 1 {
				t.Fatalf("start/final counts=%d/%d", len(admin.starts), len(admin.finals))
			}
			start, final := admin.starts[0], admin.finals[0]
			if start.RequestID != final.RequestID || start.Model != "fixture-base" || *start.AttemptCount != 1 || *start.SentAttemptCount != 0 || len(start.Attempts) != 1 || *start.Attempts[0].Sent {
				t.Fatalf("start/root projection=%+v", start)
			}
			for _, metadata := range []api.RequestLedgerMetadata{start.RequestLedgerMetadata, final.RequestLedgerMetadata, admin.startLogs[0].RequestLedgerMetadata, admin.finalLogs[0].RequestLedgerMetadata} {
				if metadata.RequestedModel != "playground/anti-truncation/fixture-alias" || metadata.BaseModel != "fixture-base" || metadata.RequestedChannel == nil || *metadata.RequestedChannel != "playground" || metadata.AntiTruncation == nil || !*metadata.AntiTruncation {
					t.Fatalf("selection projection=%+v", metadata)
				}
			}
			if start.UsageSource != "unknown" || *start.UsageKnown || len(admin.startLogs[0].Attempts) != 1 {
				t.Fatalf("start usage/attempts=%+v", admin.startLogs[0])
			}
			for _, metadata := range []api.RequestLedgerMetadata{final.RequestLedgerMetadata, admin.finalLogs[0].RequestLedgerMetadata} {
				if metadata.UsageKnown == nil || *metadata.UsageKnown != (fixture.usage != nil) || metadata.UsageSource != fixture.source || *metadata.AttemptCount != 2 || *metadata.SentAttemptCount != 2 {
					t.Fatalf("final usage/counts=%+v", metadata)
				}
			}
			for _, attempt := range admin.finalLogs[0].Attempts {
				if attempt.ParentID != final.RequestID || attempt.RequestID == final.RequestID {
					t.Fatalf("parent/choice isolation=%+v", attempt)
				}
			}
			// None of the start/final/log/queued row copies may share mutable evidence.
			*final.AntiTruncation = false
			final.Attempts[0].Usage.TotalTokens = 999
			if !*admin.finalLogs[0].AntiTruncation || admin.finalLogs[0].Attempts[0].Usage.TotalTokens != 3 || *start.Attempts[0].Sent {
				t.Fatal("log projection aliases AccessLog")
			}
			*admin.finalLogs[0].RequestedChannel = "mutated"
			if err := store.Close(); err != nil { // drains the real async write queue, no polling
				t.Fatal(err)
			}
			store, err = requestdb.Open(path, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			from, to := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(time.Minute)
			page, err := store.Records(context.Background(), api.UsageRecordQuery{From: from, To: to, Limit: 10})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("records=%+v err=%v", page, err)
			}
			row := page.Items[0]
			if row.ID != final.RequestID || row.Model != "fixture-base" || row.BaseModel != "fixture-base" || row.RequestedModel != "playground/anti-truncation/fixture-alias" || *row.RequestedChannel != "playground" || row.Channel != "playground" || !*row.AntiTruncation || *row.AttemptCount != 2 || *row.SentAttemptCount != 2 || len(row.Attempts) != 2 {
				t.Fatalf("DB projection=%+v", row)
			}
			wantTokens := int64(0)
			if fixture.usage != nil {
				wantTokens = fixture.usage.TotalTokens
			}
			if *row.UsageKnown != (fixture.usage != nil) || row.UsageSource != fixture.source || row.TotalTokens != wantTokens || row.Attempts[0].Usage.TotalTokens != 3 || row.Attempts[1].Usage.TotalTokens != 5 || row.Attempts[0].UsageSource != "upstream" || row.Attempts[1].UsageSource != "estimated" {
				t.Fatalf("root/physical usage mixed: %+v", row)
			}
			report, err := store.Usage(context.Background(), api.UsageQuery{From: from, To: to, Bucket: time.Minute, Location: time.UTC, Stack: "model"})
			if err != nil || report.Totals.Requests != 1 || report.Totals.TotalTokens != wantTokens {
				t.Fatalf("logical totals=%+v err=%v", report.Totals, err)
			}
		})
	}
}
