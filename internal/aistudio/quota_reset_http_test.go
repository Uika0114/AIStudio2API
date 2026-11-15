package aistudio

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func resetHTTPError(t *testing.T, message, header string, retrySeconds *int64) *RPCError {
	t.Helper()
	value := []any{8, message}
	if retrySeconds != nil {
		value = append(value, []any{[]any{"type.googleapis.com/google.rpc.RetryInfo", []any{[]any{*retrySeconds}}}})
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateRPCResponse("GenerateContent", &RPCResponse{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{header}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	})
	var rpcError *RPCError
	if !errors.As(err, &rpcError) {
		t.Fatalf("expected RPCError: %v", err)
	}
	return rpcError
}

func TestQuotaResetHTTPObservedMessages(t *testing.T) {
	for _, test := range []struct {
		text string
		want time.Duration
	}{
		{"13 minutes", 13 * time.Minute},
		{"12 minutes", 12 * time.Minute},
		{"21 hours, 55 minutes", 21*time.Hour + 55*time.Minute},
		{"1 hour, 1 minute", time.Hour + time.Minute},
		{"1 hour", time.Hour},
		{"1 minute", time.Minute},
		{"0 hours, 12 minutes", 12 * time.Minute},
	} {
		t.Run(test.text, func(t *testing.T) {
			message := "You've reached your usage limit for this model (resets in " + test.text + "). Retry with an alternative Gemini model or upgrade your Google AI plan for higher limits."
			got := resetHTTPError(t, message, "", nil)
			if got.RetryDelay != test.want {
				t.Fatalf("delay=%v want=%v", got.RetryDelay, test.want)
			}
			now := time.Date(2026, 11, 15, 10, 4, 47, 0, time.UTC)
			cooldown, ok := QuotaCooldownForError(got, now)
			if !ok || cooldown.Escalate || !cooldown.Until.Equal(now.Add(test.want)) {
				t.Fatalf("cooldown=%+v ok=%v", cooldown, ok)
			}
			assertQuotaLimits(t, parseDailyQuotaLimits(message, "gemini-4-argon"), -1, -1)
			raw, _ := json.Marshal([]any{8, message})
			if independent := DecodeRPCError("BidiGenerateContent", 429, raw); independent.RetryDelay != 0 {
				t.Fatal("new HTTP-only reset grammar leaked into independent decoder")
			}
		})
	}
}

func TestQuotaResetHTTPInvalidMessagesRemainUnclassified(t *testing.T) {
	for _, duration := range []string{
		"", "0 minutes", "-1 minute", "1.5 hours", "1 day", "13 seconds",
		"1 hour, -2 minutes", "2 minutes, 1 hour", "1 hour, 1 hour",
		"1 hour, 2 minutes, 3 seconds", "1 hour junk", "1 hour,",
		"9223372036854775808 hours", "2562048 hours, 1000 minutes",
	} {
		t.Run(duration, func(t *testing.T) {
			got := resetHTTPError(t, "You've reached your usage limit for this model (resets in "+duration+").", "", nil)
			if got.RetryDelay != 0 {
				t.Fatalf("invalid duration accepted: %v", got.RetryDelay)
			}
		})
	}
	for _, message := range []string{
		"Something else (resets in 13 minutes).",
		"You've reached your usage limit for this model (resets in 13 minutes",
		"You've reached your usage limit for this model (resets in 13 minutes) junk.",
	} {
		if got := resetHTTPError(t, message, "", nil); got.RetryDelay != 0 {
			t.Fatalf("unconfirmed message accepted: %q", message)
		}
	}
}

func TestQuotaResetHTTPPriorityAndLegacyRetry(t *testing.T) {
	reset := "You've reached your usage limit for this model (resets in 21 hours, 55 minutes)."
	positive, zero, negative := int64(7), int64(0), int64(-1)
	for _, test := range []struct {
		name, message, header string
		structured            *int64
		want                  time.Duration
	}{
		{"structured wins", reset, "11", &positive, 7 * time.Second},
		{"header before reset", reset, "11", nil, 11 * time.Second},
		{"header before legacy", "Please retry in 26m0.565983787s.", "11", nil, 11 * time.Second},
		{"zero structured", reset, "11", &zero, 11 * time.Second},
		{"negative structured", reset, "11", &negative, 11 * time.Second},
		{"invalid header", reset, "invalid", nil, 21*time.Hour + 55*time.Minute},
		{"zero header", reset, "0", nil, 21*time.Hour + 55*time.Minute},
		{"negative header", reset, "-1", nil, 21*time.Hour + 55*time.Minute},
		{"legacy duration", "Please retry in 26m0.565983787s.", "", nil, 26*time.Minute + 565983787*time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := resetHTTPError(t, test.message, test.header, test.structured); got.RetryDelay != test.want {
				t.Fatalf("delay=%v want=%v", got.RetryDelay, test.want)
			}
		})
	}
	until := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	got := resetHTTPError(t, reset, until.UTC().Format(http.TimeFormat), nil)
	if delta := time.Until(until) - got.RetryDelay; delta < -time.Second || delta > time.Second {
		t.Fatalf("HTTP date ignored: %v", got.RetryDelay)
	}
}
