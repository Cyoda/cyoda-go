package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// adminReq builds an httptest request with a ROLE_ADMIN UserContext attached.
// Admin handlers are gated by requireAdmin, so tests exercising positive
// paths must use this helper (or otherwise inject an admin context).
func adminReq(method, target string, body io.Reader) *http.Request {
	return withAdminCtx(httptest.NewRequest(method, target, body))
}

// withAdminCtx attaches a UserContext holding ROLE_ADMIN to the request.
// The admin handlers (M2M, key pair, trusted keys) are wrapped by the auth
// middleware in production, which guarantees a UserContext is present; the
// role-check guard then inspects that context. Tests that want to exercise
// authorised behaviour must attach an equivalent context.
func withAdminCtx(req *http.Request) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID: "test-admin",
		Roles:  []string{"ROLE_ADMIN"},
	}))
}

// withNonAdminCtx attaches a UserContext holding only ROLE_USER.
func withNonAdminCtx(req *http.Request) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID: "test-user",
		Roles:  []string{"ROLE_USER"},
	}))
}

// assertForbidden runs the handler and expects 403 Forbidden.
func assertForbidden(t *testing.T, h http.Handler, req *http.Request, desc string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("%s: expected 403, got %d (body=%q)", desc, rec.Code, rec.Body.String())
	}
}

// assertUnauthorized runs the handler and expects 401 Unauthorized.
func assertUnauthorized(t *testing.T, h http.Handler, req *http.Request, desc string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("%s: expected 401, got %d (body=%q)", desc, rec.Code, rec.Body.String())
	}
}

// --- M2M handler ---

func TestM2MHandler_NonAdminForbidden(t *testing.T) {
	handler := NewM2MHandler(NewInMemoryM2MClientStore())

	body := `{"tenantId":"t1","userId":"u1","roles":["ROLE_USER"]}`
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"create", httptest.NewRequest(http.MethodPost, "/account/m2m", bytes.NewBufferString(body))},
		{"list", httptest.NewRequest(http.MethodGet, "/account/m2m", nil)},
		{"delete", httptest.NewRequest(http.MethodDelete, "/account/m2m/some-id", nil)},
		{"reset-secret", httptest.NewRequest(http.MethodPost, "/account/m2m/some-id/secret/reset", nil)},
	}
	for _, tc := range cases {
		tc.req.Header.Set("Content-Type", "application/json")
		assertForbidden(t, handler, withNonAdminCtx(tc.req), "non-admin "+tc.name)
	}
}

func TestM2MHandler_NoUserContextUnauthorized(t *testing.T) {
	handler := NewM2MHandler(NewInMemoryM2MClientStore())

	req := httptest.NewRequest(http.MethodGet, "/account/m2m", nil)
	// Intentionally no user context — mirrors what would happen if auth
	// middleware were ever bypassed or misconfigured.
	assertUnauthorized(t, handler, req, "no-ctx list")
}

// --- Positive case: admin context allows through ---
// These guard against a future mistake where the guard rejects admins.

func TestM2MHandler_AdminCanList(t *testing.T) {
	handler := NewM2MHandler(NewInMemoryM2MClientStore())

	req := withAdminCtx(httptest.NewRequest(http.MethodGet, "/account/m2m", nil))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin list: expected 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// --- Problem-detail (RFC 9457) shape for requireAdmin ---

// decodeProblem decodes a response body as RFC 9457 ProblemDetail and
// returns the errorCode property. Fails the test if either the body is
// not problem+json or errorCode is missing.
func decodeProblemErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected Content-Type=application/problem+json, got %q (body=%q)", ct, rec.Body.String())
	}
	var pd common.ProblemDetail
	if err := json.NewDecoder(rec.Body).Decode(&pd); err != nil {
		t.Fatalf("failed to decode problem-detail: %v", err)
	}
	if pd.Props == nil {
		t.Fatalf("problem-detail missing properties bag (body=%q)", rec.Body.String())
	}
	code, ok := pd.Props["errorCode"].(string)
	if !ok || code == "" {
		t.Fatalf("problem-detail missing errorCode property (props=%v)", pd.Props)
	}
	return code
}

func TestRequireAdmin_NoUserContext_ReturnsRFC9457Unauthorized(t *testing.T) {
	handler := NewM2MHandler(NewInMemoryM2MClientStore())

	// No admin context attached — middleware bypass / misconfiguration case.
	req := httptest.NewRequest(http.MethodGet, "/account/m2m", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec); got != common.ErrCodeUnauthorized {
		t.Errorf("expected errorCode=%s, got %s", common.ErrCodeUnauthorized, got)
	}
}

func TestRequireAdmin_NonAdmin_ReturnsRFC9457Forbidden(t *testing.T) {
	handler := NewM2MHandler(NewInMemoryM2MClientStore())

	req := withNonAdminCtx(httptest.NewRequest(http.MethodGet, "/account/m2m", nil))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec); got != common.ErrCodeForbidden {
		t.Errorf("expected errorCode=%s, got %s", common.ErrCodeForbidden, got)
	}
}
