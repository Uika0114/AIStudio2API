package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func quotaMessage(dimension, limit, model string) string {
	metric := "requests"
	if dimension == "tokens" {
		metric = "content_tokens"
	}
	return fmt.Sprintf("Quota exceeded for metric: generativelanguage.googleapis.com/generate_%s_per_model_per_user, limit: %s, model: %s, description: Generate content %s per user per day limit exceeded.", metric, limit, model, dimension)
}

func quotaHeader(value string) http.Header {
	header := make(http.Header)
	if value != "" {
		header.Set("X-AIStudio-G1-Tier", value)
	}
	return header
}

func assertQuotaLimits(t *testing.T, limits *DailyQuotaLimits, requests, tokens int64) {
	t.Helper()
	if requests < 0 && tokens < 0 {
		if limits != nil {
			t.Fatalf("unexpected limits: %+v", limits)
		}
		return
	}
	if limits == nil {
		t.Fatal("missing limits")
	}
	for _, check := range []struct {
		name string
		got  *int64
		want int64
	}{{"requests", limits.Requests, requests}, {"tokens", limits.Tokens, tokens}} {
		if check.want < 0 {
			if check.got != nil {
				t.Errorf("%s must remain unknown, got %d", check.name, *check.got)
			}
		} else if check.got == nil || *check.got != check.want {
			t.Errorf("%s: got %v, want %d", check.name, check.got, check.want)
		}
	}
}

func TestDailyQuotaLimitsExactEvidence(t *testing.T) {
	token := quotaMessage("tokens", "20000000", "gemini-4-argon")
	request := quotaMessage("requests", "150", "gemini-4-argon")
	for _, test := range []struct {
		name     string
		message  string
		model    string
		requests int64
		tokens   int64
	}{
		{"argon tokens", token, "gemini-4-argon", -1, 20000000},
		{"carbon tokens", quotaMessage("tokens", "20000000", "gemini-4-carbon"), "gemini-4-carbon", -1, 20000000},
		{"requests only", request, "gemini-4-argon", 150, -1},
		{"requests with docs suffix", request + " See https://aistudio.google.com/docs/rate-limits.", "gemini-4-argon", 150, -1},
		{"tokens with docs suffix", token + " See https://aistudio.google.com/docs/rate-limits.", "gemini-4-argon", -1, 20000000},
		{"listed docs suffix", "Quota exhausted.\n* " + request + " See https://aistudio.google.com/docs/rate-limits.\nPlease retry in 3h.", "gemini-4-argon", 150, -1},
		{"minute with docs suffix", strings.Replace(request, "per day", "per minute", 1) + " See https://aistudio.google.com/docs/rate-limits.", "gemini-4-argon", -1, -1},
		{"unknown docs suffix", request + " See https://example.com/docs/rate-limits.", "gemini-4-argon", -1, -1},
		{"docs suffix with extra text", request + " See https://aistudio.google.com/docs/rate-limits. for project x", "gemini-4-argon", -1, -1},
		{"both independent", token + "\n" + request, "gemini-4-argon", 150, 20000000},
		{"normalized model", quotaMessage("tokens", "20000000", "models/gemini-4-argon"), "models/gemini-4-argon", -1, 20000000},
		{"listed message", "Quota exhausted.\n* " + token + "\r\nTry later.", "gemini-4-argon", -1, 20000000},
		{"wrong model", token, "gemini-4-carbon", -1, -1},
		{"model suffix", token, "gemini-4-argon-preview", -1, -1},
		{"no model", token, "", -1, -1},
		{"metric description mismatch", strings.Replace(token, "content_tokens", "requests", 1), "gemini-4-argon", -1, -1},
		{"minute description", strings.Replace(token, "per day", "per minute", 1), "gemini-4-argon", -1, -1},
		{"minute metric", strings.Replace(token, "per_model_per_user", "per_model_per_user_per_minute", 1), "gemini-4-argon", -1, -1},
		{"global metric", strings.Replace(token, "per_model_per_user", "per_model", 1), "gemini-4-argon", -1, -1},
		{"unknown metric", strings.Replace(token, "generate_content_tokens", "generate_unknown_tokens", 1), "gemini-4-argon", -1, -1},
		{"trailing unknown format", token + " for project x", "gemini-4-argon", -1, -1},
		{"free form", "daily tokens 20000000 gemini-4-argon", "gemini-4-argon", -1, -1},
		{"zero", quotaMessage("tokens", "0", "gemini-4-argon"), "gemini-4-argon", -1, 0},
		{"zero requests", quotaMessage("requests", "0", "gemini-4-argon"), "gemini-4-argon", 0, -1},
		{"safe maximum", quotaMessage("tokens", "9007199254740991", "gemini-4-argon"), "gemini-4-argon", -1, 9007199254740991},
		{"unsafe integer", quotaMessage("tokens", "9007199254740992", "gemini-4-argon"), "gemini-4-argon", -1, -1},
		{"integer overflow", quotaMessage("tokens", "9223372036854775808", "gemini-4-argon"), "gemini-4-argon", -1, -1},
		{"negative", quotaMessage("tokens", "-1", "gemini-4-argon"), "gemini-4-argon", -1, -1},
		{"fractional", quotaMessage("tokens", "1.5", "gemini-4-argon"), "gemini-4-argon", -1, -1},
		{"infinite", quotaMessage("tokens", "Inf", "gemini-4-argon"), "gemini-4-argon", -1, -1},
		{"duplicate equal", token + "\n" + token, "gemini-4-argon", -1, 20000000},
		{"conflicting tokens", token + "\n" + quotaMessage("tokens", "100", "gemini-4-argon"), "gemini-4-argon", -1, -1},
		{"conflict preserves requests", token + "\n" + quotaMessage("tokens", "100", "gemini-4-argon") + "\n" + request, "gemini-4-argon", 150, -1},
		{"conflict preserves tokens", request + "\n" + quotaMessage("requests", "100", "gemini-4-argon") + "\n" + token, "gemini-4-argon", -1, 20000000},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertQuotaLimits(t, parseDailyQuotaLimits(test.message, test.model), test.requests, test.tokens)
		})
	}
}

func newQuotaPhysicalAttempt(t *testing.T, tier, method string) (context.Context, *physicalAttempt, *[]GenerationAttempt) {
	t.Helper()
	snapshots := new([]GenerationAttempt)
	ctx := ContextWithGenerationAttemptObserver(context.Background(), func(value GenerationAttempt) {
		*snapshots = append(*snapshots, value)
	})
	ctx, attempt := beginPhysicalAttempt(ctx, GenerateRequest{ID: t.Name(), Model: "models/gemini-4-argon", AccountID: "fixture-account"}, RPCRequest{Method: method, Header: quotaHeader(tier), Streaming: true})
	return ctx, attempt, snapshots
}

func TestPhysicalAttemptQuotaHTTP429(t *testing.T) {
	message := quotaMessage("tokens", "20000000", "gemini-4-argon")
	raw, err := json.Marshal([]any{8, message})
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []GenerationAttempt
	ctx := ContextWithGenerationAttemptObserver(context.Background(), func(value GenerationAttempt) {
		snapshots = append(snapshots, value)
	})
	var attempt *physicalAttempt
	client, err := NewClient(ClientOptions{
		Transport: fixtureAuxiliaryRPC{},
		Protected: ProtectedTransportFunc(func(ctx context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
			ctx, attempt = beginPhysicalAttempt(ctx, request, rpc)
			markPhysicalSend(ctx, rpc.Header)
			return attempt.attach(&RPCResponse{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"3"}},
				Body:       io.NopCloser(strings.NewReader(string(raw))),
			}), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.tiers["fixture-account"] = BenefitTierPro
	_, err = client.doProtected(ctx, GenerateRequest{ID: t.Name(), Model: "gemini-4-argon", AccountID: "fixture-account"}, []byte("[]"))
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.Code != 8 || rpcError.StatusCode != 429 || rpcError.Message != message || rpcError.RetryDelay != 3*time.Second {
		t.Fatalf("error semantics changed: %v", err)
	}
	last := snapshots[len(snapshots)-1]
	if last.Stage != "http_error" || last.Error != "upstream HTTP 429" || !last.Sent || last.QuotaTier != "pro" {
		t.Fatalf("unexpected HTTP attempt: %+v", last)
	}
	assertQuotaLimits(t, last.QuotaLimits, -1, 20000000)
	if last.QuotaLimits.ObservedAt.IsZero() || last.QuotaLimits.ObservedAt != last.SentAt {
		t.Fatal("observation did not retain send time")
	}
	foundBefore, foundAfter := false, false
	var beforeRevision uint64
	for _, snapshot := range snapshots {
		if snapshot.Stage == "http_error" && snapshot.QuotaLimits == nil {
			foundBefore, beforeRevision = true, snapshot.Revision
		}
		if snapshot.QuotaLimits != nil && snapshot.Revision > beforeRevision {
			foundAfter = true
		}
	}
	if !foundBefore || !foundAfter || !attempt.terminal {
		t.Fatal("HTTP decoding failed to publish a later evidence-only revision")
	}
}

func TestPhysicalAttemptQuotaStreamAndSnapshots(t *testing.T) {
	ctx, attempt, snapshots := newQuotaPhysicalAttempt(t, "TIER2", "GenerateContent")
	markPhysicalSend(ctx)
	response := attempt.attach(&RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader("[]"))})
	if _, err := validateRPCResponse("GenerateContent", response); err != nil {
		t.Fatal(err)
	}
	message := quotaMessage("requests", "150", "gemini-4-argon")
	raw, _ := json.Marshal([]any{8, message})
	rpcError := DecodeRPCError("GenerateContent", http.StatusOK, raw)
	attempt.observe(Event{Kind: EventError, Err: rpcError})
	first := (*snapshots)[len(*snapshots)-1]
	assertQuotaLimits(t, first.QuotaLimits, 150, -1)
	if first.Stage != "failed" || first.StatusCode != 200 || !first.RateLimited || first.QuotaTier != "ultra" {
		t.Fatalf("stream classification changed: %+v", first)
	}
	attempt.decoded(rpcError, false)
	attempt.observe(Event{Kind: EventUsage, Usage: &Usage{InputTokens: 10}})
	attempt.fail(&RPCError{Code: 429, Message: quotaMessage("tokens", "20000000", "gemini-4-argon")})
	attempt.fail(&RPCError{Code: 8, Message: quotaMessage("requests", "999", "gemini-4-argon")})
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	last := (*snapshots)[len(*snapshots)-1]
	assertQuotaLimits(t, last.QuotaLimits, 150, 20000000)
	assertQuotaLimits(t, first.QuotaLimits, 150, -1)
	if last.Stage != first.Stage || last.Error != first.Error || last.QuotaLimits.ObservedAt != first.SentAt {
		t.Fatal("late evidence changed terminal state or observation time")
	}
	*last.QuotaLimits.Requests = 999
	*last.QuotaLimits.Tokens = 999
	last.QuotaLimits.ObservedAt = time.Time{}
	attempt.observe(Event{Kind: EventFinish})
	afterMutation := (*snapshots)[len(*snapshots)-1]
	assertQuotaLimits(t, afterMutation.QuotaLimits, 150, 20000000)
	if afterMutation.QuotaLimits.ObservedAt != first.SentAt {
		t.Fatal("observer mutated shared evidence")
	}
	serialized, err := json.Marshal(afterMutation)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"QuotaTier", "QuotaLimits", "ObservedAt", "quota_tier", "quota_limits", "generativelanguage.googleapis.com", "Quota exceeded", "20000000", "TIER2", "ultra"} {
		if strings.Contains(string(serialized), secret) {
			t.Fatalf("internal evidence leaked to JSON: %s", secret)
		}
	}
}

func TestPhysicalAttemptQuotaSentTierIsAuthoritative(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial string
		sent    string
		want    string
	}{
		{"same", "TIER1", "TIER1", "pro"},
		{"changed", "TIER1", "TIER2", "ultra"},
		{"removed", "TIER1", "", ""},
		{"added", "", "TIER2", "ultra"},
		{"missing", "", "", ""},
		{"unknown", "TIER2", "TIER0", ""},
		{"not exact", "TIER1", "tier1", ""},
		{"not trimmed", "TIER1", " TIER1 ", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, attempt, snapshots := newQuotaPhysicalAttempt(t, test.initial, "GenerateContent")
			markPhysicalSend(ctx, quotaHeader(test.sent))
			attempt.fail(&RPCError{StatusCode: 429, Message: quotaMessage("tokens", "0", "gemini-4-argon")})
			last := (*snapshots)[len(*snapshots)-1]
			if last.QuotaTier != test.want {
				t.Fatalf("tier %q, want %q", last.QuotaTier, test.want)
			}
			if test.want == "" {
				assertQuotaLimits(t, last.QuotaLimits, -1, -1)
			} else {
				assertQuotaLimits(t, last.QuotaLimits, -1, 0)
			}
		})
	}
	if dailyQuotaTier(http.Header{"X-Aistudio-G1-Tier": []string{"TIER1", "TIER2"}}) != "" {
		t.Fatal("ambiguous tier must be unknown")
	}
}

func TestPhysicalAttemptQuotaEligibility(t *testing.T) {
	message := quotaMessage("tokens", "20000000", "gemini-4-argon")
	for _, test := range []struct {
		name   string
		method string
		sent   bool
		err    error
		want   bool
	}{
		{"HTTP quota", "GenerateContent", true, &RPCError{StatusCode: 429, Message: message}, true},
		{"protocol quota", "GenerateContent", true, &RPCError{StatusCode: 200, Code: 8, Message: message}, true},
		{"protocol 429", "GenerateContent", true, &RPCError{StatusCode: 200, Code: 429, Message: message}, true},
		{"no quota status", "GenerateContent", true, &RPCError{StatusCode: 400, Code: 3, Message: message}, false},
		{"not sent", "GenerateContent", false, &RPCError{StatusCode: 429, Message: message}, false},
		{"build", buildProxyStreamedMethod, true, &RPCError{StatusCode: 429, Message: message}, false},
		{"assistant", assistantInitMethod, true, &RPCError{StatusCode: 429, Message: message}, false},
		{"not generation", "CountTokens", true, &RPCError{StatusCode: 429, Message: message}, false},
		{"wrong model", "GenerateContent", true, &RPCError{StatusCode: 429, Message: quotaMessage("tokens", "20000000", "gemini-4-carbon")}, false},
		{"plain error", "GenerateContent", true, errors.New(message), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, attempt, snapshots := newQuotaPhysicalAttempt(t, "TIER1", test.method)
			if test.sent {
				markPhysicalSend(ctx)
			}
			attempt.fail(test.err)
			last := (*snapshots)[len(*snapshots)-1]
			if test.want {
				assertQuotaLimits(t, last.QuotaLimits, -1, 20000000)
			} else {
				assertQuotaLimits(t, last.QuotaLimits, -1, -1)
			}
		})
	}
}

func TestPhysicalAttemptQuotaLeaseAttribution(t *testing.T) {
	pool, account := strictFixturePool(t)
	lease, _, err := pool.TryAcquireFor(context.Background(), strictSelection(ChannelPlayground))
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer lease.Release()
	var snapshot GenerationAttempt
	ctx := ContextWithGenerationAttemptObserver(ContextWithAccountLease(context.Background(), lease), func(value GenerationAttempt) { snapshot = value })
	ctx, attempt := beginPhysicalAttempt(ctx, GenerateRequest{ID: t.Name(), AccountID: "stale-request-account", Model: "gemini-4-argon"}, RPCRequest{Method: "GenerateContent", Header: quotaHeader("TIER1")})
	markPhysicalSend(ctx)
	attempt.fail(&RPCError{Code: 8, Message: quotaMessage("tokens", "20000000", "gemini-4-argon")})
	if snapshot.AccountID != account.ID {
		t.Fatalf("account %q, want lease account %q", snapshot.AccountID, account.ID)
	}
	assertQuotaLimits(t, snapshot.QuotaLimits, -1, 20000000)
}
