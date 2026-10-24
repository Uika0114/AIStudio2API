package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestAntiObservationSnapshotIdentityRevisionAndCopy(t *testing.T) {
	ctx, meta := attemptTestContext()
	value := aistudio.AntiTruncationObservation{Version: 1, ID: "lifetime", Revision: 2, RequestID: "same-id", Purpose: "compaction", Injected: true, Sent: true, End: "preparing", Rounds: []aistudio.AntiRoundObservation{{AttemptID: "physical", Calls: 1, FinishReason: "provider_19"}}}
	AddAccessLogAntiObservation(ctx, value)
	value.Rounds[0].Calls = 100
	AddAccessLogAntiObservation(ctx, aistudio.AntiTruncationObservation{ID: "lifetime", Revision: 1, RequestID: "same-id", Purpose: "compaction"})
	AddAccessLogAntiObservation(ctx, aistudio.AntiTruncationObservation{ID: "lifetime", Revision: 99, RequestID: "wrong", Purpose: "compaction"})
	AddAccessLogAntiObservation(ctx, aistudio.AntiTruncationObservation{Version: 1, ID: "reply-lifetime", Revision: 1, RequestID: "same-id", Purpose: "reply", End: "not_sent"})
	s := meta.snapshot()
	if len(s.ledger.AntiObservations) != 2 || s.ledger.AttemptCount != nil || s.ledger.SentAttemptCount != nil {
		t.Fatalf("lifecycle became physical attempt: %+v", s.ledger)
	}
	got := s.ledger.AntiObservations[0]
	if got.Revision != 2 || got.Rounds[0].Calls != 1 || got.ParentID != "http-fixture-root" || got.Complete {
		t.Fatalf("snapshot: %+v", got)
	}
	got.Rounds[0].Calls = 99
	if meta.snapshot().ledger.AntiObservations[0].Rounds[0].Calls != 1 {
		t.Fatal("snapshot aliases root")
	}
	// Freezing the root before producer cleanup must retain unknown coverage.
	frozen := meta.snapshot().ledger
	value = meta.snapshot().ledger.AntiObservations[0]
	value.Revision = 3
	value.End = "cancelled"
	value.Rounds = nil // sparse final callback cannot erase prior positive evidence
	AddAccessLogAntiObservation(ctx, value)
	if frozen.AntiObservations[0].End != "preparing" || frozen.AntiObservations[0].Complete || frozen.AntiObservations[0].Rounds[0].Calls != 1 {
		t.Fatal("late cleanup changed frozen cancellation evidence")
	}
	if current := meta.snapshot().ledger.AntiObservations[0]; current.Rounds[0].Calls != 1 || current.Complete {
		t.Fatal("sparse cancel erased evidence or forged complete")
	}
}

func TestAntiObservationConcurrentCandidatesAndKnownEmpty(t *testing.T) {
	ctx, meta := attemptTestContext()
	selection, _ := aistudio.ParseModelSelection("playground/anti-truncation/fixture")
	SetAccessLogModelSelection(ctx, selection)
	if value := meta.snapshot().ledger.AntiObservations; value == nil || len(value) != 0 {
		t.Fatal("new no-lifetime evidence conflated with legacy")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			id := fmt.Sprintf("transport-%d", i)
			for revision := uint64(1); revision <= 4; revision++ {
				AddAccessLogAntiObservation(ctx, aistudio.AntiTruncationObservation{Version: 1, ID: id, RequestID: fmt.Sprintf("choice-%d", i), Purpose: "reply", Revision: revision, Sent: true, Rounds: []aistudio.AntiRoundObservation{{AttemptID: id, Calls: 1}}})
			}
		})
	}
	wg.Wait()
	if got := meta.snapshot().ledger; len(got.AntiObservations) != 12 || got.AttemptCount != nil {
		t.Fatalf("concurrent lifetimes: %+v", got)
	}
}

type antiCancelFixture struct {
	fakeService
	entered, release chan struct{}
	hit              bool
	done             chan struct{}
}

func (s *antiCancelFixture) Generate(ctx context.Context, r aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	selection, _ := aistudio.ParseModelSelection("playground/anti-truncation/fixture")
	SetAccessLogModelSelection(ctx, selection)
	v := aistudio.AntiTruncationObservation{Version: 1, ID: "lifetime", RequestID: r.ID, Purpose: "reply", Revision: 1, Injected: true, Sent: true, End: "preparing", Rounds: []aistudio.AntiRoundObservation{{AttemptID: "physical", Calls: 0}}}
	if s.hit {
		v.Rounds[0].Calls = 1
	}
	AddAccessLogAntiObservation(ctx, v)
	events := make(chan aistudio.Event)
	go func() {
		defer close(events)
		defer close(s.done)
		close(s.entered)
		<-s.release
		v.Revision = 2
		v.Complete = true
		v.End = "finished"
		AddAccessLogAntiObservation(ctx, v)
	}()
	return events, nil
}

func TestAntiObservationRootCancelDoesNotWaitForLateFinal(t *testing.T) {
	for _, hit := range []bool{false, true} {
		t.Run(fmt.Sprint(hit), func(t *testing.T) {
			s := &antiCancelFixture{entered: make(chan struct{}), release: make(chan struct{}), hit: hit, done: make(chan struct{})}
			admin := &attemptTestAdmin{}
			handler := NewHandler(s, Config{Admin: admin})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"playground/anti-truncation/fixture","messages":[{"role":"user","content":"synthetic"}]}`)).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			returned := make(chan struct{})
			go func() { defer close(returned); handler.ServeHTTP(httptest.NewRecorder(), r) }()
			<-s.entered
			cancel()
			<-returned
			if len(admin.entries) != 1 || len(admin.entries[0].AntiObservations) != 1 {
				t.Fatal("cancel root evidence lost")
			}
			frozen := admin.entries[0].AntiObservations[0]
			want := 0
			if hit {
				want = 1
			}
			if frozen.Complete || frozen.Rounds[0].Calls != want {
				t.Fatalf("cancel forged negative: %+v", frozen)
			}
			close(s.release)
			<-s.done
			if admin.entries[0].AntiObservations[0].Complete || len(admin.entries) != 1 {
				t.Fatal("late final altered root receipt")
			}
		})
	}
}
