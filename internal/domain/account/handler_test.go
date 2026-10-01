package account_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func TestNewHandler(t *testing.T) {
	h := account.New(nil, nil, nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestAccountGet(t *testing.T) {
	h := account.New(nil, nil, nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})

	uc := &spi.UserContext{
		UserID:   "user-1",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: "tenant-1", Name: "Test Tenant"},
		Roles:    []string{"ROLE_ADMIN", "ROLE_M2M"},
	}
	ctx := spi.WithUserContext(httptest.NewRequest("GET", "/account", nil).Context(), uc)
	r := httptest.NewRequest("GET", "/account", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	h.AccountGet(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)

	info, _ := resp["userAccountInfo"].(map[string]any)
	if info == nil {
		t.Fatal("missing userAccountInfo")
	}
	if info["userId"] != "user-1" {
		t.Errorf("userId = %v, want user-1", info["userId"])
	}
	le, _ := info["legalEntity"].(map[string]any)
	if le == nil {
		t.Fatal("missing legalEntity")
	}
	if le["id"] != "tenant-1" {
		t.Errorf("legalEntity.id = %v, want tenant-1", le["id"])
	}
}

func TestAccountGetNoAuth(t *testing.T) {
	h := account.New(nil, nil, nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/account", nil)
	h.AccountGet(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandlerReturns501(t *testing.T) {
	h := account.New(nil, nil, nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})

	tests := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
	}{
		{"AccountSubscriptionsGet", func(w http.ResponseWriter, r *http.Request) {
			h.AccountSubscriptionsGet(w, r)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/test", nil)
			tt.call(w, r)
			if w.Code != http.StatusNotImplemented {
				t.Errorf("expected 501, got %d", w.Code)
			}
		})
	}
}

// TestGetTechnicalUserToken_Returns500_RoutingRegression verifies that the
// defensive stub returns 500 (not 501): arriving here means the public mux
// failed to intercept POST /oauth/token before chi could dispatch it.
func TestGetTechnicalUserToken_Returns500_RoutingRegression(t *testing.T) {
	h := account.New(nil, nil, nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/oauth/token", nil)
	h.GetTechnicalUserToken(w, r, genapi.GetTechnicalUserTokenParams{})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d; body: %s", w.Code, w.Body.String())
	}
}
