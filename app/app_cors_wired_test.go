package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// TestApp_CORSWiredUp_Preflight verifies that a fully-constructed
// app.Handler() actually has the CORS middleware installed in the
// chain. A future refactor that drops the install site would fail
// this test loud (it would 405 instead of 204).
func TestApp_CORSWiredUp_Preflight(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.CORS.Enabled = true
	cfg.CORS.Wildcard = true
	cfg.CORS.AllowedOrigins = nil
	// Force an in-memory store so the test does not require Postgres.
	cfg.StorageBackend = "memory"
	cfg.Cluster.Enabled = false

	a := app.New(cfg)
	defer a.Close()

	// Synthetic preflight against an arbitrary path. CORS middleware
	// short-circuits before any router can 404/405 — so we should see
	// 204 with the preflight headers regardless of whether the path
	// is registered.
	req := httptest.NewRequest(http.MethodOptions, "/api/entity/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Origin", "https://x.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (CORS middleware not installed?)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods missing — CORS middleware not installed?")
	}
}

// TestApp_CORSWiredUp_DisabledNoHeaders verifies that with
// CYODA_CORS_ENABLED=false the chain emits no CORS headers — the
// identity-wrapper path. A regression here would silently re-enable
// CORS in production deployments that opted out.
func TestApp_CORSWiredUp_DisabledNoHeaders(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.CORS.Enabled = false
	cfg.StorageBackend = "memory"
	cfg.Cluster.Enabled = false

	a := app.New(cfg)
	defer a.Close()

	req := httptest.NewRequest(http.MethodOptions, "/api/entity/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Origin", "https://x.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)

	// With CORS disabled, the chi router is reached and 405s the OPTIONS
	// (no preflight handling, no preflight short-circuit).
	if rec.Code == http.StatusNoContent {
		t.Errorf("status = %d (204) — CORS middleware should NOT be installed when disabled", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty when disabled", got)
	}
	if got := rec.Header().Get("Vary"); got == "Origin" {
		t.Errorf("Vary: Origin emitted while CORS is disabled")
	}
}
