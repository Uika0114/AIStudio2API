package pricing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func priceFixture(version string, input, cached, output float64) []byte {
	return []byte(fmt.Sprintf(`{"schema_version":1,"version":%q,"currency":"USD","unit_tokens":1000000,"models":{"gemini-4-carbon":{"input":%g,"cached_input":%g,"output":%g},"free-model":{"input":0,"cached_input":0,"output":0}}}`, version, input, cached, output))
}

func writePrices(t *testing.T, path string, data []byte) {
	t.Helper()
	temporary, err := os.CreateTemp(filepath.Dir(path), ".prices-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name, path); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultJSONPreservesReferencePrices(t *testing.T) {
	// Migration regression: these are the previous Go table's rates, not live overrides.
	cases := map[string][3]float64{
		"gemini-4-argon":                         {2, .10, 10},
		"gemini-3.8-flash":                       {.75, .075, 3.75},
		"gemini-3.7-flash":                       {.75, .075, 3.75},
		"gemini-3.6-flash":                       {.75, .075, 3.75},
		"gemini-3.5-flash":                       {1.5, .15, 9},
		"gemini-3.5-flash-lite":                  {.3, .03, 2.5},
		"gemini-3.1-pro-preview":                 {2, .2, 12},
		"gemini-3.1-pro-preview-customtools":     {2, .2, 12},
		"gemini-3.1-flash-lite":                  {.25, .025, 1.5},
		"gemini-3-flash-preview":                 {.5, .05, 3},
		"gemini-2.5-pro":                         {1.25, .125, 10},
		"gemini-2.5-flash":                       {.3, .03, 2.5},
		"gemini-2.5-flash-lite":                  {.1, .01, .4},
		"gemini-robotics-er-2-preview":           {1, .1, 5},
		"gemini-robotics-er-2-streaming-preview": {1, .1, 5},
	}
	snapshot := DefaultSnapshot()
	if len(snapshot.models) != len(cases) {
		t.Fatal("default model count changed")
	}
	for model, rate := range cases {
		t.Run(model, func(t *testing.T) {
			want := .09*rate[0] + .01*rate[1] + rate[2]
			if !snapshot.Priced(model) || math.Abs(snapshot.Cost(model, 100000, 10000, 1000000)-want) > 1e-12 {
				t.Fatal("old rate/formula changed")
			}
		})
	}
	for _, model := range []string{"gemini-4-carbon", "unknown"} {
		if snapshot.Priced(model) || snapshot.Cost(model, 1000000, 0, 1000000) != 0 {
			t.Fatal("unknown model must stay unpriced")
		}
	}
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-2.5-pro"} {
		price := snapshot.models[model]
		for _, input := range []int64{199999, 200000, 200001} {
			rate := price.rates
			if input > 200000 {
				rate = price.longRates
			}
			want := (float64(input-1000)*rate.input + 1000*rate.cached + 100*rate.output) / 1e6
			if snapshot.Cost(model, input, 1000, 100) != want {
				t.Fatal("long-context threshold changed")
			}
		}
	}
}

func TestPriceCatalogWholeTableReloadAndImmutableSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	catalog := NewCatalog()
	writePrices(t, path, priceFixture("one", 1, .1, 2))
	if err := catalog.Reload(path); err != nil {
		t.Fatal(err)
	}
	old := catalog.Snapshot()
	if old.Version() != "one" || old.Cost("gemini-4-carbon", 1000000, 100000, 1000000) != 2.91 || old.Priced("gemini-4-argon") {
		t.Fatal("whole table was not applied")
	}
	if !old.Priced("free-model") || old.Cost("free-model", 100, 0, 100) != 0 {
		t.Fatal("explicit free must be priced")
	}
	writePrices(t, path, priceFixture("two", 3, .3, 6))
	if err := catalog.Reload(path); err != nil {
		t.Fatal(err)
	}
	if catalog.Snapshot().Version() != "two" || old.Version() != "one" || old.Cost("gemini-4-carbon", 1000000, 100000, 1000000) != 2.91 {
		t.Fatal("old report snapshot mutated")
	}
	data := catalog.Snapshot().JSON()
	data[0] = '!'
	status := catalog.PricingStatus()
	status.PriceList[0] = '!'
	if catalog.Snapshot().JSON()[0] != '{' || status.SHA256 != catalog.Snapshot().SHA256() || status.Source != path || status.ModelCount != 2 {
		t.Fatal("mutable status or bad source metadata")
	}
}

func TestPriceCatalogUTF8BOMKeepsFileIdentityAndValidManagementJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	data := append([]byte{0xef, 0xbb, 0xbf}, priceFixture("bom", 1, .1, 2)...)
	writePrices(t, path, data)
	catalog := NewCatalog()
	if err := catalog.Reload(path); err != nil {
		t.Fatal(err)
	}
	if catalog.Snapshot().Version() != "bom" || !json.Valid(catalog.Snapshot().JSON()) {
		t.Fatal("UTF-8 BOM not normalized")
	}
	if _, err := json.Marshal(catalog.PricingStatus()); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if catalog.Snapshot().SHA256() != hex.EncodeToString(hash[:]) {
		t.Fatal("status must identify original file bytes")
	}
}

func TestPriceCatalogRejectsInvalidWholeTablesAndKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	catalog := NewCatalog()
	valid := string(priceFixture("good", 1, .1, 2))
	writePrices(t, path, []byte(valid))
	if err := catalog.Reload(path); err != nil {
		t.Fatal(err)
	}
	old := catalog.Snapshot()
	cases := map[string]string{
		"syntax":             "{",
		"currency":           strings.Replace(valid, `"USD"`, `"CNY"`, 1),
		"unit":               strings.Replace(valid, `1000000`, `1000`, 1),
		"schema":             strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
		"negative":           strings.Replace(valid, `"input":1`, `"input":-1`, 1),
		"missing":            strings.Replace(valid, `"cached_input":0.1,`, "", 1),
		"null":               strings.Replace(valid, `"output":2`, `"output":null`, 1),
		"overflow":           strings.Replace(valid, `"input":1`, `"input":1e300`, 1),
		"nan":                strings.Replace(valid, `"input":1`, `"input":NaN`, 1),
		"unknown_field":      strings.Replace(valid, `"input":1`, `"wrong":1,"input":1`, 1),
		"duplicate_rate":     strings.Replace(valid, `"input":1`, `"input":1,"input":2`, 1),
		"case_shadow":        strings.Replace(valid, `"input":1`, `"input":1,"Input":2`, 1),
		"duplicate_model":    strings.Replace(valid, `"free-model":`, `"gemini-4-carbon":`, 1),
		"trailing_json":      valid + " {}",
		"long_missing_rate":  strings.Replace(valid, `"output":2`, `"output":2,"long_context":{"input_tokens_above":200000,"input":4}`, 1),
		"long_bad_threshold": strings.Replace(valid, `"output":2`, `"output":2,"long_context":{"input_tokens_above":0,"input":4,"cached_input":0.4,"output":8}`, 1),
		"oversize":           strings.Repeat(" ", MaxFileBytes+1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			writePrices(t, path, []byte(data))
			if err := catalog.Reload(path); err == nil || catalog.Snapshot() != old || catalog.PricingStatus().LastError == "" {
				t.Fatal("invalid table replaced active snapshot")
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reload(path); err == nil || catalog.Snapshot() != old {
		t.Fatal("missing file must preserve active table")
	}
	cold := NewCatalog()
	if err := cold.Reload(path); err == nil || cold.Snapshot().Version() != DefaultSnapshot().Version() || cold.PricingStatus().LastError == "" {
		t.Fatal("cold fallback must be explicit")
	}
	writePrices(t, path, []byte(valid))
	if err := catalog.Reload(path); err != nil || catalog.PricingStatus().LastError != "" {
		t.Fatal("recovering same good content must clear error")
	}
}

func TestPriceCatalogWatchReloadErrorRecoveryAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	catalog := NewCatalog()
	one := priceFixture("one", 1, .1, 2)
	writePrices(t, path, one)
	if err := catalog.Reload(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Status, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		catalog.Watch(ctx, path, 5*time.Millisecond, func(status Status) { events <- status })
	}()
	wait := func(version string, bad bool) {
		t.Helper()
		select {
		case status := <-events:
			if status.Version != version || (status.LastError != "") != bad {
				t.Fatalf("bad reload event: %+v", status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("watch event timeout")
		}
	}
	writePrices(t, path, []byte("{"))
	wait("one", true)
	select {
	case <-events:
		t.Fatal("same error was repeatedly emitted")
	case <-time.After(30 * time.Millisecond):
	}
	writePrices(t, path, one)
	wait("one", false)
	writePrices(t, path, priceFixture("two", 2, .2, 4))
	wait("two", false)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}
