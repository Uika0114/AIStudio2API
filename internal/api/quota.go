package api

import (
	"context"
	"net/http"
	"time"
)

type QuotaService interface {
	Quota(context.Context) (QuotaReport, error)
}

type QuotaBalance struct {
	Limit     *int64 `json:"limit"`
	Used      *int64 `json:"used"`
	Remaining *int64 `json:"remaining"`
}

type QuotaMember struct {
	ID      string
	Label   string
	Tier    string
	State   string
	Enabled bool
}

type QuotaAccount struct {
	ID           string       `json:"id"`
	Label        string       `json:"label"`
	Tier         string       `json:"tier"`
	State        string       `json:"state"`
	Included     bool         `json:"included"`
	Exhausted429 bool         `json:"exhausted_429"`
	InFlight     int64        `json:"in_flight"`
	Unreported   int64        `json:"unreported"`
	Requests     QuotaBalance `json:"requests"`
	Tokens       QuotaBalance `json:"tokens"`
}

type QuotaReport struct {
	Channel          string         `json:"channel"`
	Model            string         `json:"model"`
	Method           string         `json:"method"`
	PeriodFrom       time.Time      `json:"period_from"`
	ResetAt          time.Time      `json:"reset_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	ObservedSince    time.Time      `json:"observed_since"`
	Coverage         string         `json:"coverage"`
	ExcludedAccounts int            `json:"excluded_accounts"`
	InFlight         int64          `json:"in_flight"`
	Unreported       int64          `json:"unreported"`
	Requests         QuotaBalance   `json:"requests"`
	Tokens           QuotaBalance   `json:"tokens"`
	History          *QuotaHistory  `json:"history,omitempty"`
	Accounts         []QuotaAccount `json:"accounts"`
}
type QuotaHistory struct {
	From       time.Time `json:"from"`
	Until      time.Time `json:"until"`
	Attempts   int64     `json:"attempts"`
	Skipped    int64     `json:"skipped"`
	TimeSource string    `json:"time_source"`
}

func (s *server) handleQuota(w http.ResponseWriter, r *http.Request) {
	report, err := s.config.Quota.Quota(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"message": "额度估算暂不可用"}})
		return
	}
	writeJSON(w, http.StatusOK, report)
}
