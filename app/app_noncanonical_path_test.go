package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// TestApp_NonCanonicalPath_NeverReachesRouter pins the mount the
// administration-route predicate (internal/adminroute) depends on: the API
// router is chi, which routes an empty or dot segment, but the ServeMux
// mounted above it answers a non-canonical path with a redirect to the
// canonical one — with and without a context path — so chi never sees
// PUT /model//1/lock, and the predicate's "not administration" for such a path
// is never the answer a token-carrying request ends on.
func TestApp_NonCanonicalPath_NeverReachesRouter(t *testing.T) {
	for _, tc := range []struct{ contextPath, path, want string }{
		{"/api", "/api/model//1/lock", "/api/model/1/lock"},
		{"/api", "/api/model/order/1/../1/lock", "/api/model/order/1/lock"},
		{"", "/model//1/lock", "/model/1/lock"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := app.DefaultConfig()
			cfg.StorageBackend = "memory"
			cfg.Cluster.Enabled = false
			cfg.ContextPath = tc.contextPath
			a := app.New(cfg)
			t.Cleanup(func() {
				a.Shutdown()
				_ = a.Close()
			})

			req := httptest.NewRequest(http.MethodPut, tc.path, nil)
			req.Header.Set("X-Tx-Token", "not-a-transaction-token")
			rec := httptest.NewRecorder()
			a.Handler().ServeHTTP(rec, req)
			// chi would answer 401 (auth first) or 404/405; only the ServeMux
			// above it redirects, and it redirects a non-canonical path with
			// 307 (net/http findHandler: RedirectHandler with
			// StatusTemporaryRedirect).
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d body=%s; want 307 from the ServeMux above the router", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}
