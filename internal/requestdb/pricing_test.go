package requestdb

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/pricing"
)

func priceUsageFixture(t *testing.T) (*Store, api.UsageQuery) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.db")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i := range 16 {
		store.Record(Row{ID: fmt.Sprintf("carbon-%d", i), Time: from.Add(time.Duration(i*5) * time.Minute), Model: "gemini-4-carbon", State: "completed", Status: 200, InputTokens: 1000000, ReplyTokens: 1000000, TotalTokens: 2000000})
	}
	store.Record(Row{ID: "previous", Time: from.Add(-time.Hour), Model: "gemini-4-carbon", State: "completed", Status: 200, InputTokens: 1000000, ReplyTokens: 1000000, TotalTokens: 2000000})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, api.UsageQuery{From: from, To: from.Add(2 * time.Hour), Bucket: time.Hour, Location: time.UTC, Stack: "model"}
}

func priceUsageFile(t *testing.T, version string, input, output float64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices.json")
	data := fmt.Sprintf(`{"schema_version":1,"version":%q,"currency":"USD","unit_tokens":1000000,"models":{"gemini-4-carbon":{"input":%g,"cached_input":0,"output":%g}}}`, version, input, output)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func priceHistoryRows(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT id,time,model,input_tokens,cached_tokens,reasoning_tokens,reply_tokens,total_tokens FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id, model string
		var at, input, cached, reasoning, reply, total int64
		if err := rows.Scan(&id, &at, &model, &input, &cached, &reasoning, &reply, &total); err != nil {
			t.Fatal(err)
		}
		result = append(result, fmt.Sprint(id, at, model, input, cached, reasoning, reply, total))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPricingHistoricalUsageRecalculatesWithoutRewritingRows(t *testing.T) {
	store, query := priceUsageFixture(t)
	ctx := context.Background()
	beforeRows := priceHistoryRows(t, store)
	unpriced, err := store.Usage(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if unpriced.Cost != 0 || !reflect.DeepEqual(unpriced.UnpricedModels, []string{"gemini-4-carbon"}) {
		t.Fatal("Carbon default must remain explicitly unpriced")
	}
	catalog := pricing.NewCatalog()
	one := priceUsageFile(t, "one", 1, 2)
	if err := catalog.Reload(one); err != nil {
		t.Fatal(err)
	}
	store.SetPricing(catalog)
	first, err := store.Usage(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cost != 48 || first.PreviousCost != 3 || len(first.UnpricedModels) != 0 || first.PricingVersion != "one" || first.PricingSHA256 != catalog.Snapshot().SHA256() {
		t.Fatalf("first prices not applied: %+v", first)
	}
	two := priceUsageFile(t, "two", 3, 6)
	if err := catalog.Reload(two); err != nil {
		t.Fatal(err)
	}
	second, err := store.Usage(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cost != 144 || second.PreviousCost != 9 || second.PricingVersion != "two" {
		t.Fatal("historical estimate did not hot update")
	}
	if !reflect.DeepEqual(beforeRows, priceHistoryRows(t, store)) || !reflect.DeepEqual(first.Totals, second.Totals) {
		t.Fatal("repricing changed ledger rows/usage")
	}
	status := store.PricingStatus()
	var table map[string]any
	if err := json.Unmarshal(status.PriceList, &table); err != nil || status.Version != "two" {
		t.Fatal("management status is not effective table")
	}
}

func TestPricingConcurrentWholeReportsNeverMixVersions(t *testing.T) {
	store, query := priceUsageFixture(t)
	catalog := pricing.NewCatalog()
	a := priceUsageFile(t, "price-a", 1, 2)
	b := priceUsageFile(t, "price-b", 3, 6)
	if err := catalog.Reload(a); err != nil {
		t.Fatal(err)
	}
	store.SetPricing(catalog)
	hashes := map[string]string{"price-a": catalog.Snapshot().SHA256()}
	if err := catalog.Reload(b); err != nil {
		t.Fatal(err)
	}
	hashes["price-b"] = catalog.Snapshot().SHA256()
	stop := make(chan struct{})
	done := make(chan struct{})
	fail := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			for _, path := range []string{a, b} {
				select {
				case <-stop:
					return
				default:
				}
				if err := catalog.Reload(path); err != nil {
					fail <- err
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
		select {
		case err := <-fail:
			t.Error(err)
		default:
		}
	}()
	for range 60 {
		report, err := store.Usage(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		perRow := float64(3)
		if report.PricingVersion == "price-b" {
			perRow = 9
		} else if report.PricingVersion != "price-a" {
			t.Fatal("unknown report price identity")
		}
		if report.PricingSHA256 != hashes[report.PricingVersion] || report.Cost != 16*perRow || report.PreviousCost != perRow {
			t.Fatalf("report mixed prices: %s current=%g previous=%g", report.PricingVersion, report.Cost, report.PreviousCost)
		}
		var bucketCost float64
		for _, bucket := range report.Buckets {
			bucketCost += bucket.Cost
		}
		if math.Abs(bucketCost-report.Cost) > 1e-9 {
			t.Fatal("buckets mixed price versions")
		}
	}
}
