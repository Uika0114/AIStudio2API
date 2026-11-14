package aistudio

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DailyQuotaLimits contains only explicitly observed per-model daily limits.
// ObservedAt follows the physical send, so delayed replies cannot look newer.
type DailyQuotaLimits struct {
	Requests   *int64
	Tokens     *int64
	ObservedAt time.Time
}

func cloneDailyQuotaLimits(limits *DailyQuotaLimits) *DailyQuotaLimits {
	if limits == nil {
		return nil
	}
	copied := *limits
	if limits.Requests != nil {
		value := *limits.Requests
		copied.Requests = &value
	}
	if limits.Tokens != nil {
		value := *limits.Tokens
		copied.Tokens = &value
	}
	return &copied
}

func dailyQuotaTier(header http.Header) string {
	values := header.Values("X-AIStudio-G1-Tier")
	if len(values) != 1 {
		return ""
	}
	switch values[0] {
	case "TIER1":
		return "pro"
	case "TIER2":
		return "ultra"
	default:
		return ""
	}
}

// Deliberately recognize only the two upstream message formats confirmed in
// production. In particular, do not infer daily limits from quota metadata,
// minute/global quotas, or a description without its matching metric.
var dailyQuotaLine = regexp.MustCompile(`^(?:\* )?Quota exceeded for metric: generativelanguage\.googleapis\.com/generate_(content_tokens|requests)_per_model_per_user, limit: ([0-9]+), model: ([^,\s]+), description: Generate content (tokens|requests) per user per day limit exceeded\.(?: See https://aistudio\.google\.com/docs/rate-limits\.)?$`)

func parseDailyQuotaLimits(message, baseModel string) *DailyQuotaLimits {
	baseModel = strings.TrimPrefix(strings.TrimSpace(baseModel), "models/")
	if baseModel == "" {
		return nil
	}
	var requests, tokens *int64
	var requestsConflict, tokensConflict bool
	for _, line := range strings.Split(message, "\n") {
		fields := dailyQuotaLine.FindStringSubmatch(strings.TrimSpace(line))
		if fields == nil || strings.TrimPrefix(fields[3], "models/") != baseModel {
			continue
		}
		var target **int64
		var conflict *bool
		switch {
		case fields[1] == "content_tokens" && fields[4] == "tokens":
			target, conflict = &tokens, &tokensConflict
		case fields[1] == "requests" && fields[4] == "requests":
			target, conflict = &requests, &requestsConflict
		default:
			continue
		}
		limit, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || limit >= 1<<53 {
			continue
		}
		if *target != nil && **target != limit {
			*conflict = true
		}
		*target = &limit
	}
	if requestsConflict {
		requests = nil
	}
	if tokensConflict {
		tokens = nil
	}
	if requests == nil && tokens == nil {
		return nil
	}
	return &DailyQuotaLimits{Requests: requests, Tokens: tokens}
}

// captureQuotaError runs inside update's lock. It only adds evidence and never
// changes scheduling, error classification, or the physical attempt's terminal.
func (attempt *physicalAttempt) captureQuotaError(value *GenerationAttempt, err error) {
	if !value.Sent || value.Channel != ChannelPlayground || value.Method != "GenerateContent" ||
		(value.QuotaTier != "pro" && value.QuotaTier != "ultra") {
		return
	}
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError == nil ||
		(rpcError.StatusCode != http.StatusTooManyRequests && rpcError.Code != 8 && rpcError.Code != 429) {
		return
	}
	limits := parseDailyQuotaLimits(rpcError.Message, value.BaseModel)
	if limits == nil {
		return
	}
	if value.QuotaLimits == nil {
		limits.ObservedAt = value.SentAt
		value.QuotaLimits = limits
		return
	}
	// Replayed or late snapshots may fill a second dimension, but cannot replace
	// a previously observed value for this same physical attempt.
	if value.QuotaLimits.Requests == nil {
		value.QuotaLimits.Requests = limits.Requests
	}
	if value.QuotaLimits.Tokens == nil {
		value.QuotaLimits.Tokens = limits.Tokens
	}
}

func (attempt *physicalAttempt) observeQuotaError(err error) {
	if attempt == nil || err == nil {
		return
	}
	attempt.update(func(value *GenerationAttempt) {
		attempt.captureQuotaError(value, err)
	})
}
