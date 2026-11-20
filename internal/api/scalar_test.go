package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalapi "github.com/cyoda/cyoda-go/internal/api"
)

func TestScalarUIEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	internalapi.RegisterDiscoveryRoutes(mux, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/docs")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html, got %s", ct)
	}
}

func TestOpenAPISpecEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	internalapi.RegisterDiscoveryRoutes(mux, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
}
