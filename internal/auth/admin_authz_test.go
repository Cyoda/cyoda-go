package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// withNonAdminCtx attaches a UserContext holding only ROLE_USER.
func withNonAdminCtx(req *http.Request) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID: "test-user",
		Roles:  []string{"ROLE_USER"},
	}))
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !RequireAdmin(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// No admin context attached — middleware bypass / misconfiguration case.
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !RequireAdmin(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := withNonAdminCtx(httptest.NewRequest(http.MethodGet, "/probe", nil))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec); got != common.ErrCodeForbidden {
		t.Errorf("expected errorCode=%s, got %s", common.ErrCodeForbidden, got)
	}
}
