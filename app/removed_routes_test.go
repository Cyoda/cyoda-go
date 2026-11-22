package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file holds the tests that prove removed routes stay removed. It names
// the removed paths, so the documentation exit checks exclude it.

// TestJWTMode_NoOIDCRoutes: the provider endpoints do not exist.
func TestJWTMode_NoOIDCRoutes(t *testing.T) {
	a, key := jwtAppWithKey(t)
	req := httptest.NewRequest(http.MethodGet, "/oauth/oidc/providers", nil)
	req.Header.Set("Authorization", "Bearer "+mintAppToken(t, key, "ROLE_ADMIN", "ROLE_M2M"))
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /oauth/oidc/providers: status %d, want 404/405", rr.Code)
	}
}
