package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/api"
)

func withRoles(req *http.Request, roles ...string) *http.Request {
	uc := &spi.UserContext{
		UserID: "caller",
		Tenant: spi.Tenant{ID: "acme", Name: "acme"},
		Roles:  roles,
	}
	return req.WithContext(spi.WithUserContext(req.Context(), uc))
}

// TestRequireM2M covers the three outcomes: no principal → 401, a principal
// without ROLE_M2M → 403 before the handler runs, ROLE_M2M → the handler runs.
func TestRequireM2M(t *testing.T) {
	cases := []struct {
		name       string
		req        *http.Request
		wantStatus int
		wantCode   string
		wantCalled bool
	}{
		{"no principal", httptest.NewRequest(http.MethodGet, "/x", nil), http.StatusUnauthorized, "UNAUTHORIZED", false},
		{"admin without ROLE_M2M", withRoles(httptest.NewRequest(http.MethodGet, "/x", nil), "ROLE_ADMIN"), http.StatusForbidden, "FORBIDDEN", false},
		{"no roles", withRoles(httptest.NewRequest(http.MethodGet, "/x", nil)), http.StatusForbidden, "FORBIDDEN", false},
		{"ROLE_M2M", withRoles(httptest.NewRequest(http.MethodGet, "/x", nil), "ROLE_M2M"), http.StatusNoContent, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := api.RequireM2M(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, tc.req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if called != tc.wantCalled {
				t.Fatalf("handler called=%v, want %v", called, tc.wantCalled)
			}
			if tc.wantCode != "" && !strings.Contains(rr.Body.String(), tc.wantCode) {
				t.Fatalf("body %s lacks error code %s", rr.Body.String(), tc.wantCode)
			}
			if tc.wantStatus == http.StatusForbidden && !strings.Contains(rr.Body.String(), "this operation requires ROLE_M2M") {
				t.Fatalf("body %s lacks the ROLE_M2M cause", rr.Body.String())
			}
		})
	}
}

// TestChiMux_GuardsEveryRouteOutsideTheAllowList registers one allow-listed
// and one other pattern through the ServeMux interface the generated router
// uses: the allow-listed one is reachable without ROLE_M2M, the other is not.
func TestChiMux_GuardsEveryRouteOutsideTheAllowList(t *testing.T) {
	mux := api.NewChiMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("GET /account", ok)
	mux.HandleFunc("GET /account/subscriptions", ok)
	mux.HandleFunc("GET /entity/{entityId}", ok)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/account", http.StatusNoContent},
		{"/account/subscriptions", http.StatusForbidden},
		{"/entity/00000000-0000-0000-0000-000000000001", http.StatusForbidden},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, withRoles(httptest.NewRequest(http.MethodGet, tc.path, nil), "ROLE_ADMIN"))
		if rr.Code != tc.want {
			t.Errorf("GET %s without ROLE_M2M: status %d, want %d", tc.path, rr.Code, tc.want)
		}
		rr = httptest.NewRecorder()
		mux.ServeHTTP(rr, withRoles(httptest.NewRequest(http.MethodGet, tc.path, nil), "ROLE_M2M"))
		if rr.Code != http.StatusNoContent {
			t.Errorf("GET %s with ROLE_M2M: status %d, want 204", tc.path, rr.Code)
		}
	}
}

func TestIsAllowListed(t *testing.T) {
	for _, tc := range []struct {
		method, pattern string
		want            bool
	}{
		{"GET", "/account", true},
		{"GET", "/account/subscriptions", false},
		{"POST", "/clients", true},
		{"GET", "/oauth/keys/keypair/current", true},
		{"GET", "/entity/{entityId}", false},
		{"get", "/account", false}, // methods are matched as registered
	} {
		if got := api.IsAllowListed(tc.method, tc.pattern); got != tc.want {
			t.Errorf("IsAllowListed(%q, %q) = %v, want %v", tc.method, tc.pattern, got, tc.want)
		}
	}
}
