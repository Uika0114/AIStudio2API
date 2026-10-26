package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func assistantBudgetEnvelope(status int) []byte {
	step := make([]any, 17)
	step[0] = []any{[]any{[]any{[]any{nil, "fixture complete text longer than budget one"}}, "model"}}
	step[5] = []any{"fixture step failure"}
	step[11], step[12], step[13], step[15], step[16] = 1, status, "models/gemini-4-argon", []any{"10", "5", "2"}, "FIXTURE-GEN"
	data, _ := json.Marshal([]any{[]any{[]any{nil, nil, []any{step}, "FIXTURE-APPLET"}}})
	return data
}

// Deliberately delivers no public event. These tests cover decoder/pre-first-event
// cancellation, NOT cancellation after an HTTP body write.
type assistantBudgetBlockedBody struct {
	ctx    context.Context
	onRead func()
	once   sync.Once
}

func (b *assistantBudgetBlockedBody) Read([]byte) (int, error) {
	b.once.Do(func() {
		if b.onRead != nil {
			b.onRead()
		}
	})
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*assistantBudgetBlockedBody) Close() error { return nil }

func TestAssistantOutputBudgetTrackedFailurePreservation(t *testing.T) {
	for _, mode := range []string{"missing_root", "missing_terminal", "step_failure", "deadline", "cancel_before_event"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newAssistantBudgetFixture(t, 2)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture.tracked.timeout = 150 * time.Millisecond
			fixture.stream = func(ctx context.Context) *aistudio.RPCResponse {
				response := &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}}
				if mode == "deadline" || mode == "cancel_before_event" {
					body := &assistantBudgetBlockedBody{ctx: ctx}
					if mode == "cancel_before_event" {
						body.onRead = cancel
					}
					response.Body = body
				} else {
					status := 1
					if mode == "missing_terminal" {
						status = 2
					}
					if mode == "step_failure" {
						status = 4
					}
					data := assistantBudgetEnvelope(status)
					if mode == "missing_root" {
						data = data[:len(data)-1]
					}
					response.Body = io.NopCloser(bytes.NewReader(data))
				}
				return response
			}
			request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"assistant/gemini-4-argon","max_tokens":1,"messages":[{"role":"user","content":"fixture"}]}`)).WithContext(parent)
			request.Header.Set("Content-Type", "application/json")
			result := httptest.NewRecorder()
			fixture.handler.ServeHTTP(result, request)
			wantStatus := http.StatusBadGateway
			if mode == "deadline" {
				wantStatus = http.StatusGatewayTimeout
			}
			if mode == "cancel_before_event" {
				// A disconnected caller receives no response write. Recorder's default
				// 200 is not a delivered success; assert the actual root log terminal.
				fixture.admin.mu.Lock()
				finals := append([]api.AccessLog(nil), fixture.admin.finals...)
				fixture.admin.mu.Unlock()
				if len(finals) != 1 || finals[0].Status != 499 || result.Body.Len() != 0 {
					t.Fatalf("cancel terminal/write: %+v body=%s", finals, result.Body.String())
				}
			} else if result.Code != wantStatus {
				t.Fatalf("%s: HTTP=%d want=%d %s", mode, result.Code, wantStatus, result.Body.String())
			}
			if len(fixture.compatibilityLogs()) != 1 {
				t.Fatal("failed execution marker missing/duplicated")
			}
			fixture.preparer.mu.Lock()
			calls := fixture.preparer.calls
			fixture.preparer.mu.Unlock()
			if calls != 1 {
				t.Fatalf("failure replayed: %d submits", calls)
			}
			// The HTTP handler may return as soon as its caller cancels; the
			// core owns bounded Applet cleanup and tracked owns the pool lease.
			// Wait for both owners, not Recorder completion, before checking them.
			deadline := time.Now().Add(2 * time.Second)
			var counts map[string]int
			for {
				fixture.mu.Lock()
				methods := append([]string(nil), fixture.methods...)
				fixture.mu.Unlock()
				counts = map[string]int{}
				for _, method := range methods {
					counts[method]++
				}
				if fixture.registry.count() == 0 && counts["DeleteApplet"] == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("cleanup did not settle: active=%d methods=%v", fixture.registry.count(), counts)
				}
				time.Sleep(time.Millisecond)
			}
			if counts["CreateApplet"] != 1 || counts["DeleteApplet"] != 1 || counts["CancelCodeAssistantOfflineGeneration"] != 1 {
				t.Fatalf("resource cleanup: %v", counts)
			}
			var accountIDs []string
			for _, account := range fixture.pool.Status() {
				accountIDs = append(accountIDs, account.ID)
			}
			for id, state := range fixture.pool.CandidateStates(accountIDs, "gemini-4-argon") {
				if state.Active != 0 || state.AvailableSlot != 1 {
					t.Fatalf("owning layer did not release account %s: %+v", id, state)
				}
			}
		})
	}
}

func TestAssistantOutputBudgetHTTPCompactionReplyFailure(t *testing.T) {
	fixture := newAssistantBudgetFixture(t, 2)
	fixture.preparer.failReply = true
	payload := map[string]any{"model": "assistant/gemini-4-argon", "max_output_tokens": 1, "input": []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 9000)}, map[string]any{"role": "assistant", "content": strings.Repeat("past answer ", 9000)}, map[string]any{"role": "user", "content": "current"}}, "context_management": []any{map[string]any{"type": "compaction", "compact_threshold": 1}}}
	result := budgetAppPost(t, fixture.handler, "/v1/responses", payload)
	if result.Code == http.StatusOK || strings.Contains(result.Body.String(), `"status":"completed"`) {
		t.Fatalf("reply failure became success: %d %s", result.Code, result.Body.String())
	}
	logs := fixture.compatibilityLogs()
	if len(logs) != 2 || !strings.Contains(logs[0].Message, "purpose=compaction") || !strings.Contains(logs[1].Message, "purpose=reply") {
		t.Fatalf("execution markers=%+v", logs)
	}
	fixture.preparer.mu.Lock()
	calls := fixture.preparer.calls
	fixture.preparer.mu.Unlock()
	if calls != 2 {
		t.Fatalf("summary/reply submits=%d", calls)
	}
	fixture.admin.mu.Lock()
	finals := append([]api.AccessLog(nil), fixture.admin.finals...)
	fixture.admin.mu.Unlock()
	if len(finals) != 1 {
		t.Fatalf("root count=%d", len(finals))
	}
	physical := map[string]bool{}
	for _, attempt := range finals[0].Attempts {
		if attempt.Kind == "generation" {
			physical[attempt.ID] = true
		}
	}
	if len(physical) != 2 {
		t.Fatalf("physical attempts=%d", len(physical))
	}
}

func TestAssistantOutputBudgetTrackedFixedTimeout(t *testing.T) {
	fixture := newAssistantBudgetFixture(t, 1)
	for _, value := range []int64{1, 65536, 1 << 62, math.MaxInt64} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			request := aistudio.GenerateRequest{Model: "gemini-4-argon", Selection: aistudio.ModelSelection{BaseModel: "gemini-4-argon", Channel: aistudio.ChannelAssistant}, Config: aistudio.GenerationConfig{MaxOutputTokens: &value}}
			if got := fixture.tracked.generateTimeout(request); got != 3*time.Second {
				t.Fatalf("budget changed Assistant timeout: %s", got)
			}
		})
	}
}

func TestAssistantOutputBudgetHTTPReadOnlyAndHistory(t *testing.T) {
	fixture := newAssistantBudgetFixture(t, 1)
	first := budgetAppPost(t, fixture.handler, "/v1/responses", map[string]any{"model": "assistant/gemini-4-argon", "input": "first", "max_output_tokens": 1})
	if first.Code != 200 {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	var object map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	id, _ := object["id"].(string)
	if id == "" || object["max_output_tokens"] != nil {
		t.Fatal("missing saved response or misleading budget")
	}
	get := httptest.NewRecorder()
	fixture.handler.ServeHTTP(get, httptest.NewRequest("GET", "/v1/responses/"+id, nil))
	if get.Code != 200 {
		t.Fatalf("GET: %d %s", get.Code, get.Body.String())
	}
	_ = budgetAppPost(t, fixture.handler, "/v1/responses/"+id+"/cancel", map[string]any{}) // Completed response may reject cancellation.
	if len(fixture.compatibilityLogs()) != 1 {
		t.Fatal("read/cancel logged a new execution")
	}
	fixture.preparer.mu.Lock()
	calls := fixture.preparer.calls
	fixture.preparer.mu.Unlock()
	if calls != 1 {
		t.Fatal("read/cancel generated")
	}
	next := budgetAppPost(t, fixture.handler, "/v1/responses", map[string]any{"previous_response_id": id, "input": "next"})
	if next.Code != 200 {
		t.Fatalf("history: %d %s", next.Code, next.Body.String())
	}
	if len(fixture.compatibilityLogs()) != 1 {
		t.Fatal("omitted history budget was inherited")
	}
	fixture.preparer.mu.Lock()
	calls = fixture.preparer.calls
	fixture.preparer.mu.Unlock()
	if calls != 2 {
		t.Fatal("history did not generate exactly once")
	}
}
