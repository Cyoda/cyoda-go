package account_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func TestNewHandler(t *testing.T) {
	h := account.New(nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestAccountGet(t *testing.T) {
	h := account.New(nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})

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
	h := account.New(nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/account", nil)
	h.AccountGet(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandlerReturns501(t *testing.T) {
	h := account.New(nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})

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

// TestGetTechnicalUserToken_MockMode_Returns501 verifies the generated
// router's POST /oauth/token handler. It is reached only in mock IAM mode: in
// JWT IAM mode the token handler on the public mux takes every method on the
// path first. Mock mode issues no token, so the answer is 501 NOT_IMPLEMENTED,
// and the expected path logs nothing at WARN or above.
func TestGetTechnicalUserToken_MockMode_Returns501(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := account.New(nil, nil, nil, auth.IAMFeatures{}, auth.OperatorGuard{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/oauth/token", nil)
	h.GetTechnicalUserToken(w, r, genapi.GetTechnicalUserTokenParams{})

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; body: %s", err, w.Body.String())
	}
	if code, _ := body.Properties["errorCode"].(string); code != common.ErrCodeNotImplemented {
		t.Errorf("errorCode = %q, want %q", code, common.ErrCodeNotImplemented)
	}
	const wantDetail = "NOT_IMPLEMENTED: token issuance requires JWT IAM mode"
	if body.Detail != wantDetail {
		t.Errorf("detail = %q, want %q", body.Detail, wantDetail)
	}
	if logs.Len() != 0 {
		t.Errorf("logged at WARN or above on the expected path:\n%s", logs.String())
	}
}
