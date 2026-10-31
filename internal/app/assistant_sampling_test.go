package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// This suite tests sampling/wire/log ownership, not async account persistence.
// Use supported in-memory runtime state so a post-finish qualification writer
// cannot race t.TempDir cleanup; persistent-account behavior stays in its suites.
func newAssistantSamplingFixture(t *testing.T, count int) *assistantBudgetFixture {
	fixture := newAssistantBudgetFixture(t, count)
	for _, status := range fixture.pool.Status() {
		account, err := fixture.pool.Account(status.ID)
		if err != nil {
			t.Fatal(err)
		}
		account.RuntimePath = ""
	}
	return fixture
}

func samplingLogs(f *assistantBudgetFixture) []api.AdminLog {
	f.registry.mu.Lock()
	defer f.registry.mu.Unlock()
	var result []api.AdminLog
	for _, log := range f.registry.logs {
		if log.Event == "request.progress" && strings.HasPrefix(log.Message, "assistant_sampling_parameters_ignored ") {
			result = append(result, log)
		}
	}
	return result
}

func samplingChatPayload() map[string]any {
	return map[string]any{"model": "assistant/gemini-4-argon", "temperature": 0.8, "top_p": 0.9, "seed": 0, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "private-sampling-sentinel"}}}
}

func TestAssistantSamplingTrackedExecution(t *testing.T) {
	for _, mode := range []string{"valid", "omitted", "invalid", "other_invalid", "pre_send_retry", "accepted_failure", "choices"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newAssistantSamplingFixture(t, 2)
			if mode == "choices" {
				fixture.pool.SetPerAccountConcurrency(2)
			} // permit the fixture's concurrent choices without changing product scheduling
			fixture.backend.retryBeforeSend = mode == "pre_send_retry"
			fixture.preparer.failAfterSend = mode == "accepted_failure"
			payload := samplingChatPayload()
			if mode == "omitted" {
				delete(payload, "temperature")
				delete(payload, "top_p")
				delete(payload, "seed")
			}
			if mode == "invalid" {
				payload["top_p"] = 1.1
			}
			if mode == "other_invalid" {
				payload["reasoning_effort"] = "not-an-effort"
			}
			if mode == "choices" {
				payload["n"] = 2
			}
			response := budgetAppPost(t, fixture.handler, "/v1/chat/completions", payload)
			invalid := mode == "invalid" || mode == "other_invalid"
			want := 1
			if invalid || mode == "omitted" {
				want = 0
			}
			if mode == "choices" {
				want = 2
			}
			logs := samplingLogs(fixture)
			if len(logs) != want {
				t.Fatalf("marker count=%d want=%d HTTP=%d %s", len(logs), want, response.Code, response.Body.String())
			}
			for _, log := range logs {
				if log.Message != "assistant_sampling_parameters_ignored fields=temperature,top_p,seed purpose=reply" || strings.Contains(log.Message, "private-sampling-sentinel") {
					t.Fatalf("wrong marker %s", log.Message)
				}
			}
			if invalid && response.Code != 400 {
				t.Fatalf("invalid accepted: %d %s", response.Code, response.Body.String())
			}
			if !invalid && mode != "accepted_failure" && response.Code != 200 {
				t.Fatalf("valid rejected: %d %s", response.Code, response.Body.String())
			}
			if mode == "accepted_failure" && response.Code == 200 {
				t.Fatal("unknown submit became success")
			}
			fixture.preparer.mu.Lock()
			sends := fixture.preparer.calls
			fixture.preparer.mu.Unlock()
			wantSends := 1
			if invalid {
				wantSends = 0
			}
			if mode == "choices" {
				wantSends = 2
			}
			if sends != wantSends {
				t.Fatalf("submit count=%d want=%d", sends, wantSends)
			}
			fixture.backend.mu.Lock()
			calls := fixture.backend.calls
			fixture.backend.mu.Unlock()
			if mode == "pre_send_retry" && calls != 2 {
				t.Fatal("outer retry not exercised")
			}
			fixture.mu.Lock()
			for _, request := range fixture.requests {
				c := request.Config
				if c.Temperature != nil || c.TopP != nil || c.TopK != nil || c.Seed != nil {
					t.Error("uncleared config reached protected send")
				}
			}
			if invalid {
				for _, method := range fixture.methods {
					if method != "ListModels" {
						t.Errorf("invalid generated resource: %s", method)
					}
				}
			}
			fixture.mu.Unlock()
			fixture.admin.mu.Lock()
			finals := append([]api.AccessLog(nil), fixture.admin.finals...)
			fixture.admin.mu.Unlock()
			if len(finals) != 1 {
				t.Fatal("wrong root log count")
			}
			parameters := requestLogData(finals[0]).Parameters
			if mode != "omitted" && (parameters["temperature"] != "0.8" || parameters["top_p"] != fmt.Sprint(payload["top_p"])) {
				t.Fatalf("input log rewritten: %v", parameters)
			}
			if _, ok := parameters["seed"]; ok {
				t.Fatal("added raw seed logging")
			}
			if _, ok := parameters["top_k"]; ok {
				t.Fatal("added raw top_k logging")
			}
			if mode == "choices" {
				var object struct {
					Choices []json.RawMessage `json:"choices"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil || len(object.Choices) != 2 {
					t.Fatal("choice control flow changed")
				}
			}
			if fixture.registry.count() != 0 {
				t.Fatal("execution leaked")
			}
		})
	}
}

func TestAssistantSamplingTrackedFourFieldsAndPurposes(t *testing.T) {
	fixture := newAssistantSamplingFixture(t, 1)
	temperature, topP, topK, seed := 0.8, 0.9, 40, int64(-1)
	request := aistudio.GenerateRequest{ID: "same-id", Model: "assistant/gemini-4-argon", Config: aistudio.GenerationConfig{Temperature: &temperature, TopP: &topP, TopK: &topK, Seed: &seed}, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "fixture"}}}}}
	for _, purpose := range []string{"compaction", "reply"} {
		events, err := fixture.tracked.Generate(aistudio.ContextWithGenerationPurpose(t.Context(), purpose), request)
		if err != nil {
			t.Fatal(err)
		}
		for event := range events {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
		}
	}
	logs := samplingLogs(fixture)
	if len(logs) != 2 || logs[0].Message != "assistant_sampling_parameters_ignored fields=temperature,top_p,top_k,seed purpose=compaction" || logs[1].Message != "assistant_sampling_parameters_ignored fields=temperature,top_p,top_k,seed purpose=reply" {
		t.Fatalf("wrong fields/purpose: %+v", logs)
	}
	if temperature != 0.8 || topP != 0.9 || topK != 40 || seed != -1 {
		t.Fatal("caller pointer target changed")
	}
}

func TestAssistantSamplingHTTPAutoCompaction(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			fixture := newAssistantSamplingFixture(t, 1)
			payload := map[string]any{"model": "assistant/gemini-4-argon", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 9000)}, map[string]any{"role": "assistant", "content": strings.Repeat("past answer ", 9000)}, map[string]any{"role": "user", "content": "current"}}, "context_management": []any{map[string]any{"type": "compaction", "compact_threshold": 1}}}
			if explicit {
				payload["temperature"], payload["top_p"] = 0.8, 0.9
			}
			result := budgetAppPost(t, fixture.handler, "/v1/responses", payload)
			if result.Code != 200 || !strings.Contains(result.Body.String(), `"type":"compaction"`) {
				t.Fatalf("no auto-compaction: %d %s", result.Code, result.Body.String())
			}
			want := 0
			if explicit {
				want = 2
			}
			logs := samplingLogs(fixture)
			if len(logs) != want {
				t.Fatalf("wrong execution count=%d", len(logs))
			}
			if explicit && (!strings.Contains(logs[0].Message, "purpose=compaction") || !strings.Contains(logs[1].Message, "purpose=reply")) {
				t.Fatal("wrong summary/reply attribution")
			}
			fixture.preparer.mu.Lock()
			purposes := append([]string(nil), fixture.preparer.purposes...)
			fixture.preparer.mu.Unlock()
			if len(purposes) != 2 || purposes[0] != "compaction" || purposes[1] != "reply" {
				t.Fatal("wrong physical executions")
			}
		})
	}
}

func TestAssistantSamplingNoGenerationAndHistory(t *testing.T) {
	fixture := newAssistantSamplingFixture(t, 1)
	first := budgetAppPost(t, fixture.handler, "/v1/responses", map[string]any{"model": "assistant/gemini-4-argon", "input": "first", "temperature": 0.8, "top_p": 0.9})
	if first.Code != 200 {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	var object map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &object)
	id, _ := object["id"].(string)
	if id == "" || object["temperature"] != nil || object["top_p"] != nil {
		t.Fatal("invalid stored shell")
	}
	get := httptest.NewRecorder()
	fixture.handler.ServeHTTP(get, httptest.NewRequest("GET", "/v1/responses/"+id, nil))
	if get.Code != 200 {
		t.Fatal("GET failed")
	}
	_ = budgetAppPost(t, fixture.handler, "/v1/responses/"+id+"/cancel", map[string]any{})
	compact := budgetAppPost(t, fixture.handler, "/v1/responses/compact", map[string]any{"model": "assistant/gemini-4-argon", "input": "short", "temperature": 2.1, "top_p": 1.1})
	if compact.Code != 200 || len(samplingLogs(fixture)) != 1 {
		t.Fatalf("non-generating compact gained validation/generation: %d %s", compact.Code, compact.Body.String())
	}
	next := budgetAppPost(t, fixture.handler, "/v1/responses", map[string]any{"previous_response_id": id, "input": "next"})
	if next.Code != 200 || len(samplingLogs(fixture)) != 1 {
		t.Fatalf("history inherited sampling: %d %s", next.Code, next.Body.String())
	}
	fixture.preparer.mu.Lock()
	defer fixture.preparer.mu.Unlock()
	if fixture.preparer.calls != 2 {
		t.Fatal("read/cancel/no-op compact generated")
	}
}

func TestAssistantSamplingTrackedDeadlineAndCancel(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel_before_event"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newAssistantSamplingFixture(t, 2)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture.tracked.timeout = 150 * time.Millisecond // same deadline path, no 300s sleep
			fixture.stream = func(ctx context.Context) *aistudio.RPCResponse {
				body := &assistantBudgetBlockedBody{ctx: ctx}
				if mode == "cancel_before_event" {
					body.onRead = cancel
				}
				return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: body}
			}
			payload, _ := json.Marshal(samplingChatPayload())
			request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(payload))).WithContext(parent)
			request.Header.Set("Content-Type", "application/json")
			result := httptest.NewRecorder()
			fixture.handler.ServeHTTP(result, request)
			if mode == "deadline" && result.Code != 504 {
				t.Fatalf("deadline became %d %s", result.Code, result.Body.String())
			}
			if mode == "cancel_before_event" && result.Body.Len() != 0 {
				t.Fatal("cancel wrote a response")
			}
			deadline := time.Now().Add(3 * time.Second)
			for fixture.registry.count() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(samplingLogs(fixture)) != 1 || fixture.registry.count() != 0 {
				t.Fatal("cancel marker/leaked execution")
			}
			fixture.preparer.mu.Lock()
			calls := fixture.preparer.calls
			fixture.preparer.mu.Unlock()
			if calls != 1 {
				t.Fatal("unknown outcome replayed")
			}
			assertSamplingCleanup(t, fixture)
		})
	}
}

func TestAssistantSamplingTrackedProtocolFailures(t *testing.T) {
	for _, mode := range []string{"missing_root", "missing_terminal", "step_failure"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newAssistantSamplingFixture(t, 2)
			fixture.stream = func(context.Context) *aistudio.RPCResponse {
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
				return &aistudio.RPCResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(data))}
			}
			result := budgetAppPost(t, fixture.handler, "/v1/chat/completions", samplingChatPayload())
			if result.Code != 502 || len(samplingLogs(fixture)) != 1 {
				t.Fatalf("failure hidden: %d %s", result.Code, result.Body.String())
			}
			fixture.preparer.mu.Lock()
			calls := fixture.preparer.calls
			fixture.preparer.mu.Unlock()
			if calls != 1 {
				t.Fatal("post-submit failure replayed")
			}
			assertSamplingCleanup(t, fixture)
		})
	}
}

func assertSamplingCleanup(t *testing.T, fixture *assistantBudgetFixture) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fixture.mu.Lock()
		counts := map[string]int{}
		for _, method := range fixture.methods {
			counts[method]++
		}
		fixture.mu.Unlock()
		if fixture.registry.count() == 0 && counts["DeleteApplet"] == 1 {
			if counts["CreateApplet"] != 1 || counts["CancelCodeAssistantOfflineGeneration"] != 1 {
				t.Fatalf("wrong cleanup: %v", counts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not settle: %v", counts)
		}
		time.Sleep(time.Millisecond)
	}
	var ids []string
	for _, account := range fixture.pool.Status() {
		ids = append(ids, account.ID)
	}
	for id, state := range fixture.pool.CandidateStates(ids, "gemini-4-argon") {
		if state.Active != 0 || state.AvailableSlot != 1 {
			t.Fatalf("lease leaked %s: %+v", id, state)
		}
	}
}
