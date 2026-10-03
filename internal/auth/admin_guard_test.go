package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// oboUC is an on-behalf-of principal of tenant: a user stated by a client,
// holding every role an administrator would.
func oboUC(tenant string) *spi.UserContext {
	return &spi.UserContext{
		UserID: "alice", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: spi.TenantID(tenant)},
		Roles:    []string{"ROLE_ADMIN", "ROLE_M2M"},
		Executor: &spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService},
	}
}

// assertOBORefused checks the 403 FORBIDDEN an on-behalf-of principal gets
// from an administration guard.
func assertOBORefused(t *testing.T, ok bool, w *httptest.ResponseRecorder) {
	t.Helper()
	if ok || w.Code != http.StatusForbidden {
		t.Fatalf("guard admitted an OBO principal (ok %v, status %d)", ok, w.Code)
	}
	if got := guardErrorCode(t, w.Body.Bytes()); got != "FORBIDDEN" {
		t.Errorf("errorCode = %q, want FORBIDDEN", got)
	}
	if got := guardProblemDetail(t, w.Body.Bytes()); !strings.Contains(got, "on-behalf-of tokens cannot administer") {
		t.Errorf("detail = %q, want it to name on-behalf-of tokens", got)
	}
}

func TestRequireAdmin_RefusesExecutor(t *testing.T) {
	w := httptest.NewRecorder()
	assertOBORefused(t, auth.RequireAdmin(w, guardRequest(oboUC("acme"))), w)
}

func TestOperatorGuard_RefusesExecutor(t *testing.T) {
	w := httptest.NewRecorder()
	assertOBORefused(t, auth.OperatorGuard{}.Require(w, guardRequest(oboUC("PLATFORM"))), w)
}

func TestMockOperatorGuard_RefusesExecutor(t *testing.T) {
	w := httptest.NewRecorder()
	assertOBORefused(t, auth.MockOperatorGuard().Require(w, guardRequest(oboUC("mock-tenant"))), w)
}

// An admin client with no executor still passes RequireAdmin: the refusal is
// about the executor alone.
func TestRequireAdmin_AdminClientPasses(t *testing.T) {
	uc := oboUC("acme")
	uc.Executor = nil
	uc.Kind = spi.PrincipalService
	w := httptest.NewRecorder()
	if !auth.RequireAdmin(w, guardRequest(uc)) {
		t.Fatalf("RequireAdmin refused an admin client (status %d)", w.Code)
	}
}
