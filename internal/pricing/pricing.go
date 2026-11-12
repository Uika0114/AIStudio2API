// Package pricing uses immutable, versioned price lists for reference USD costs.
package pricing

import (
	_ "embed"
	"encoding/json"
	"sync"
	"time"
)

//go:embed prices.json
var defaultData []byte

type rates struct {
	input, cached, output float64
}

type modelPrice struct {
	rates
	longRates rates
	threshold int64
}

// Snapshot is immutable: one report must keep this pointer for all cost scans.
// Prices are USD per million tokens; output includes reasoning and reply tokens.
type Snapshot struct {
	version, sha256, source string
	loadedAt                time.Time
	models                  map[string]modelPrice
	raw                     json.RawMessage
}

func (snapshot *Snapshot) Version() string { return snapshot.version }
func (snapshot *Snapshot) SHA256() string  { return snapshot.sha256 }

// JSON returns a copy, never a mutable reference to the active price list.
func (snapshot *Snapshot) JSON() []byte { return append([]byte(nil), snapshot.raw...) }

func (snapshot *Snapshot) Priced(model string) bool {
	_, ok := snapshot.models[model]
	return ok
}

// Cost preserves the existing formula and strict input > threshold boundary.
// Unknown models return zero numerically and must also be marked unpriced.
func (snapshot *Snapshot) Cost(model string, input, cached, output int64) float64 {
	price := snapshot.models[model]
	value := price.rates
	if price.threshold > 0 && input > price.threshold {
		value = price.longRates
	}
	return (float64(input-cached)*value.input + float64(cached)*value.cached + float64(output)*value.output) / 1e6
}

var builtin = sync.OnceValue(func() *Snapshot {
	snapshot, err := parse(defaultData, "embedded:prices.json", time.Now().UTC())
	if err != nil {
		panic("invalid embedded reference price list: " + err.Error())
	}
	return snapshot
})

// DefaultSnapshot is the shipped JSON fallback, not a mutable process-wide table.
func DefaultSnapshot() *Snapshot { return builtin() }

// Priced and Cost retain default-list helpers; live reports use a captured catalog snapshot.
func Priced(model string) bool { return DefaultSnapshot().Priced(model) }
func Cost(model string, input, cached, output int64) float64 {
	return DefaultSnapshot().Cost(model, input, cached, output)
}
