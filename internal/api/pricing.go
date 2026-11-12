package api

import (
	"net/http"

	"github.com/Mag1cFall/AIStudio2API/internal/pricing"
)

// PricingService exposes only the effective reference table and reload status.
type PricingService interface {
	PricingStatus() pricing.Status
}

func (s *server) handlePricing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.config.Pricing.PricingStatus())
}
