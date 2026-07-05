package mock_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/api/middleware"

	mockiam "github.com/cyoda-platform/cyoda-go/internal/iam/mock"
	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestMockIAMAuthenticatesEveryRequest(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	var captured *spi.UserContext
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = spi.GetUserContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(middleware.Auth(a.AuthenticationService())(testHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("mock IAM returned 401 — should accept all requests")
	}

	if captured == nil {
		t.Fatal("UserContext not set by mock IAM")
	}
}

func TestMockIAMUserContextHasRequiredFields(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	var captured *spi.UserContext
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = spi.GetUserContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(middleware.Auth(a.AuthenticationService())(testHandler))
	defer srv.Close()

	http.Get(srv.URL + "/anything")

	if captured.Tenant.ID == "" {
		t.Error("UserContext missing Tenant.ID")
	}
	if captured.UserID == "" {
		t.Error("UserContext missing UserID")
	}
	if len(captured.Roles) == 0 {
		t.Error("UserContext missing Roles")
	}
}

// Default mock IAM must grant ROLE_M2M so clients running against a
// mock-auth server can connect to the gRPC streaming endpoint (which requires
// ROLE_M2M). Regression test for: "PERMISSION_DENIED: ROLE_M2M required for
// streaming" when no env overrides were set.
func TestMockIAMDefaultUserHasM2MAndAdminRoles(t *testing.T) {
	a := app.New(app.DefaultConfig())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	uc, err := a.AuthenticationService().Authenticate(context.Background(), req)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	if !spi.HasRole(uc.Roles, "ROLE_M2M") {
		t.Errorf("default mock user missing ROLE_M2M (roles=%v) — gRPC streaming will be denied", uc.Roles)
	}
	if !spi.HasRole(uc.Roles, "ROLE_ADMIN") {
		t.Errorf("default mock user missing ROLE_ADMIN (roles=%v) — admin endpoints will be denied", uc.Roles)
	}
}

func TestMockIAMReturnsDefensiveCopy(t *testing.T) {
	defaultUser := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: "test-tenant", Name: "Test Tenant"},
		Roles:    []string{"ROLE_USER", "ROLE_ADMIN"},
	}
	svc := mockiam.NewAuthenticationService(defaultUser)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	uc1, err := svc.Authenticate(context.Background(), req)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}

	// Mutate the returned UserContext.
	uc1.UserID = "mutated"
	uc1.Roles[0] = "ROLE_MUTATED"
	uc1.Roles = append(uc1.Roles, "ROLE_EXTRA")

	// Subsequent call should return unmodified values.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	uc2, err := svc.Authenticate(context.Background(), req2)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}

	if uc2.UserID != "test-user" {
		t.Errorf("expected UserID=test-user, got %s", uc2.UserID)
	}
	if len(uc2.Roles) != 2 {
		t.Errorf("expected 2 roles, got %d", len(uc2.Roles))
	}
	if uc2.Roles[0] != "ROLE_USER" {
		t.Errorf("expected first role=ROLE_USER, got %s", uc2.Roles[0])
	}
}
