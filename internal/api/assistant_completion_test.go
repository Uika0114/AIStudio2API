package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The real pooled/client fixture sends every byte of an in-band-complete
// Assistant response but withholds HTTP EOF. End-to-end here means only local
// adapter + client code: all RPCs are in memory, not live model requests.
type assistantCompletionRPCBody struct {
	data      *bytes.Reader
	ctx       context.Context
	closed    chan struct{}
	once      sync.Once
	tailReads atomic.Int32
}

func (body *assistantCompletionRPCBody) Read(buffer []byte) (int, error) {
	if len(buffer) > 7 {
		buffer = buffer[:7]
	}
	if n, err := body.data.Read(buffer); n > 0 || err != io.EOF {
		return n, err
	}
	body.tailReads.Add(1)
	select {
	case <-body.closed:
		return 0, io.EOF
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	}
}

func (body *assistantCompletionRPCBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

func TestAssistantCompletionThreeProtocolsWithoutTransportEOF(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, trailer := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/trailer=%v", protocol, stream, trailer), func(t *testing.T) {
					var rpcBody *assistantCompletionRPCBody
					service := newAssistantTextIntegration(t, func(ctx context.Context, wire []byte) io.ReadCloser {
						if trailer {
							var root []any
							if err := json.Unmarshal(wire, &root); err != nil {
								panic(err)
							}
							root = append(root, []any{0})
							var err error
							wire, err = json.Marshal(root)
							if err != nil {
								panic(err)
							}
						}
						rpcBody = &assistantCompletionRPCBody{data: bytes.NewReader(wire), ctx: ctx, closed: make(chan struct{})}
						return rpcBody
					})
					handler := newRoutingTestHandler(t, service)
					path, payload := assistantCompletionPayload(protocol, stream)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					done := make(chan struct{})
					go func() { defer close(done); handler.ServeHTTP(response, request) }()
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						cancel()
						<-done
						t.Fatal("protocol response did not close without transport EOF")
					}
					body := response.Body.String()
					if response.Code != http.StatusOK || !strings.Contains(body, "fixture text response") {
						t.Fatalf("status=%d body=%s", response.Code, body)
					}
					if stream {
						switch protocol {
						case "chat":
							if strings.Count(body, "data: [DONE]") != 1 || !strings.Contains(body, `"finish_reason":"stop"`) {
								t.Fatalf("missing/duplicate Chat terminal: %s", body)
							}
						case "responses":
							if strings.Count(body, "event: response.completed\n") != 1 || strings.Contains(body, "response.failed") {
								t.Fatalf("missing/duplicate Responses terminal: %s", body)
							}
						case "gemini":
							if strings.Count(body, `"finishReason":"STOP"`) != 1 {
								t.Fatalf("missing/duplicate Gemini terminal: %s", body)
							}
						}
					}
					if rpcBody == nil || rpcBody.tailReads.Load() != 0 {
						t.Fatal("decoder tried to read beyond its closed root array")
					}
					select {
					case <-rpcBody.closed:
					default:
						t.Fatal("upstream body not closed after protocol completion")
					}
					service.mu.Lock()
					submits := len(service.bodies)
					service.mu.Unlock()
					if submits != 1 {
						t.Fatalf("submits=%d", submits)
					}
				})
			}
		}
	}
}

func TestAssistantCompletionThreeProtocolsRejectBrokenEnvelope(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				service := newAssistantTextIntegration(t, func(_ context.Context, wire []byte) io.ReadCloser {
					return io.NopCloser(bytes.NewReader(wire[:len(wire)-1]))
				})
				handler := newRoutingTestHandler(t, service)
				path, payload := assistantCompletionPayload(protocol, stream)
				response := doJSON(t, handler, http.MethodPost, path, payload)
				body := response.Body.String()
				if !strings.Contains(body, "根数组没有正常结束") || strings.Contains(body, "response.completed") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, `"finishReason":"STOP"`) {
					t.Fatalf("broken envelope was hidden: status=%d body=%s", response.Code, body)
				}
				if !stream && response.Code == http.StatusOK {
					t.Fatal("broken unary response reported success")
				}
			})
		}
	}
}

func assistantCompletionPayload(protocol string, stream bool) (string, string) {
	switch protocol {
	case "chat":
		return "/v1/chat/completions", fmt.Sprintf(`{"model":"assistant/gemini-4-argon","messages":[{"role":"user","content":"fixture"}],"stream":%t}`, stream)
	case "responses":
		return "/v1/responses", fmt.Sprintf(`{"model":"assistant/gemini-4-argon","input":"fixture","stream":%t}`, stream)
	default:
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/assistant/gemini-4-argon:" + action, `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
	}
}
