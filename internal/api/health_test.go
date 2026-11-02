package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/health"
)

func TestHealthEndpointUp(t *testing.T) {
	healthFlag := health.NewLatch()

	mux := http.NewServeMux()
	internalapi.RegisterHealthRoutes(mux, healthFlag)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "UP" {
		t.Errorf("expected status UP, got %s", body["status"])
	}
}

func TestHealthEndpointDown(t *testing.T) {
	healthFlag := health.NewLatch()
	healthFlag.Trip()

	mux := http.NewServeMux()
	internalapi.RegisterHealthRoutes(mux, healthFlag)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "DOWN" {
		t.Errorf("expected status DOWN, got %s", body["status"])
	}
}
