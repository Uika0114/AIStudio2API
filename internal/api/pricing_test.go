package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/pricing"
)

func TestPricingManagementReadOnlyAndAdminAuthentication(t *testing.T) {
	catalog := pricing.NewCatalog()
	handler := NewHandler(nil, Config{Pricing: catalog, AdminAuthEnabled: true, AdminUsername: "operator", AdminPassword: "test-password"})
	if response := authExchange(handler, "GET", "/api/pricing", ""); response.Code != http.StatusUnauthorized {
		t.Fatal("pricing bypassed admin authentication")
	}
	login := authExchange(handler, "POST", "/api/auth/login", `{"username":"operator","password":"test-password"}`)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatal("test login failed")
	}
	cookie := login.Result().Cookies()[0]
	response := authExchange(handler, "GET", "/api/pricing", "", cookie)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	var status pricing.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Version != catalog.Snapshot().Version() || status.SHA256 != catalog.Snapshot().SHA256() || status.ModelCount != 15 || len(status.PriceList) == 0 {
		t.Fatal("effective price status missing")
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if response := authExchange(handler, method, "/api/pricing", "{}", cookie); response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("unexpected write route %s: %d", method, response.Code)
		}
	}
	if catalog.Snapshot().SHA256() != status.SHA256 {
		t.Fatal("management read changed price list")
	}
	if response := authExchange(handler, "GET", "/v1/pricing", "", cookie); response.Code != http.StatusNotFound {
		t.Fatal("price management exposed as generation API")
	}
}

func TestPricingManagementRetainsLoopbackOnlyMode(t *testing.T) {
	handler := NewHandler(nil, Config{Pricing: pricing.NewCatalog()})
	request := httptest.NewRequest("GET", "http://localhost/api/pricing", nil)
	request.RemoteAddr = "192.0.2.1:5000"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("nonloopback price management accepted")
	}
}
