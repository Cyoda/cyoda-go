package e2e_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"

	// Register stock storage plugins so spi.GetPlugin("memory") resolves.
	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newCORSTestServer builds a minimal app and wraps it in an httptest.Server.
// Each test gets its own fresh server with the requested CORS config.
// Uses an in-memory store and disables cluster so no external infra is needed.
func newCORSTestServer(t *testing.T, cors app.CORSConfig) (*httptest.Server, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.CORS = cors
	cfg.StorageBackend = "memory"
	cfg.Cluster.Enabled = false

	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv, func() {
		srv.Close()
		a.Shutdown()
		_ = a.Close()
	}
}

// representativeRoutes is one path per group covered by the unified
// CORS middleware. Routes are chosen from those definitely registered
// in mock-IAM / memory-backend mode. We assert on CORS headers, not
// on the body or status of the underlying handler — preflights
// short-circuit before auth and routing; actual GETs may legitimately
// 200/401/404.
//
// Context: DefaultConfig has ContextPath="/api", so all API routes
// have the /api prefix. Health is registered on the inner mux and is
// accessible at /api/health after context-path stripping.
// The /oauth/token endpoint is only registered in jwt mode (authSvc !=
// nil); in default mock mode it is absent, so we cover that group via
// /api/account instead.
var representativeRoutes = []struct {
	name   string
	method string
	path   string
}{
	{"entity-get", http.MethodGet, "/api/entity/00000000-0000-0000-0000-000000000000"},
	{"search-post", http.MethodPost, "/api/search/direct/example/1"},
	{"messaging-post", http.MethodPost, "/api/message/new/test"},
	{"account-get", http.MethodGet, "/api/account"},
	{"admin-post", http.MethodPost, "/api/admin/log-level"},
	{"help-get", http.MethodGet, "/api/help"},
	{"health-get", http.MethodGet, "/api/health"},
}

func TestCORS_E2E_PreflightAcrossGroups_LoopbackMode(t *testing.T) {
	srv, cleanup := newCORSTestServer(t, app.CORSConfig{Enabled: true})
	defer cleanup()

	for _, r := range representativeRoutes {
		t.Run(r.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodOptions, srv.URL+r.path, nil)
			req.Header.Set("Origin", "http://localhost:3000")
			req.Header.Set("Access-Control-Request-Method", r.method)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("status = %d, want 204", resp.StatusCode)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
				t.Errorf("ACAO = %q", got)
			}
			if got := resp.Header.Get("Access-Control-Allow-Methods"); got == "" {
				t.Error("missing Access-Control-Allow-Methods")
			}
			if got := resp.Header.Get("Vary"); got != "Origin" {
				t.Errorf("Vary = %q, want \"Origin\"", got)
			}
		})
	}
}

func TestCORS_E2E_LoopbackRejectsRemoteOrigin(t *testing.T) {
	srv, cleanup := newCORSTestServer(t, app.CORSConfig{Enabled: true})
	defer cleanup()

	// Actual (non-preflight) GET against /api/help — unauthenticated,
	// easy to assert headers on.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/help", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty (remote origin rejected in loopback mode)", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want \"Origin\"", got)
	}
}

func TestCORS_E2E_AllowlistMode(t *testing.T) {
	srv, cleanup := newCORSTestServer(t, app.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://admin.example.com"},
	})
	defer cleanup()

	t.Run("allowed origin", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/help", nil)
		req.Header.Set("Origin", "https://admin.example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
			t.Errorf("ACAO = %q", got)
		}
	})

	t.Run("loopback not auto-allowed", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/help", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("ACAO = %q, want empty (allowlist mode, loopback not auto-allowed)", got)
		}
	})
}

func TestCORS_E2E_WildcardMode(t *testing.T) {
	srv, cleanup := newCORSTestServer(t, app.CORSConfig{
		Enabled:  true,
		Wildcard: true,
	})
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/help", nil)
	req.Header.Set("Origin", "https://anything.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want \"*\"", got)
	}
}

func TestCORS_E2E_DisabledNoHeaders(t *testing.T) {
	srv, cleanup := newCORSTestServer(t, app.CORSConfig{Enabled: false})
	defer cleanup()

	// Preflight against /api/help — would 204 if CORS were installed,
	// must NOT 204 when disabled (no preflight short-circuit).
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/help", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		t.Errorf("status = 204, want non-204 (CORS disabled — no preflight short-circuit)")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty when disabled", got)
	}
	if got := resp.Header.Get("Vary"); got == "Origin" {
		t.Errorf("Vary: Origin emitted while disabled")
	}
}
