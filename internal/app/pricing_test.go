package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/pricing"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

func TestPricingAppHTTPHotReloadWithoutRestartOrLedgerRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.db")
	store, err := requestdb.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.Record(requestdb.Row{ID: "synthetic-carbon", Time: time.Now().Add(-5 * time.Minute), Model: "gemini-4-carbon", State: "completed", Status: 200, InputTokens: 1000000, ReplyTokens: 1000000, TotalTokens: 2000000})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = requestdb.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pricePath := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(pricePath, []byte(`{"schema_version":1,"version":"native-one","currency":"USD","unit_tokens":1000000,"models":{"gemini-4-argon":{"input":2,"cached_input":0.1,"output":10}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	startPriceList(ctx, store, pricePath, nil)
	handler := api.NewHandler(nil, api.Config{Ledger: store, Pricing: store})
	get := func(path string, destination any) {
		t.Helper()
		request := httptest.NewRequest("GET", "http://localhost"+path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), destination) != nil {
			t.Fatalf("native pricing GET %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	var status pricing.Status
	get("/api/pricing", &status)
	if status.Version != "native-one" || status.Source != pricePath || status.LastError != "" {
		t.Fatal("startup did not wire file catalog")
	}
	usagePath := "/api/usage?from=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + "&to=" + time.Now().Add(time.Minute).UTC().Format(time.RFC3339) + "&bucket=3600"
	var report api.UsageReport
	get(usagePath, &report)
	if report.Cost != 0 || len(report.UnpricedModels) != 1 || report.PricingVersion != "native-one" {
		t.Fatal("unpriced Carbon lost")
	}
	// The same native app service and ledger remain in place while only the file changes.
	staged := pricePath + ".next"
	if err := os.WriteFile(staged, []byte(`{"schema_version":1,"version":"native-two","currency":"USD","unit_tokens":1000000,"models":{"gemini-4-carbon":{"input":1,"cached_input":0,"output":2}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, pricePath); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		get("/api/pricing", &status)
		if status.Version == "native-two" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.Version != "native-two" || status.LastError != "" {
		t.Fatal("native app price watcher did not update")
	}
	get(usagePath, &report)
	if report.Cost != 3 || report.Totals.Requests != 1 || len(report.UnpricedModels) != 0 || report.PricingVersion != status.Version || report.PricingSHA256 != status.SHA256 {
		t.Fatal("native usage did not reprice existing record")
	}
}
