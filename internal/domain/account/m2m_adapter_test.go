package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- fixture helpers ---

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

func newM2MAdapterFixture(t *testing.T, flagOn bool) *Handler {
	t.Helper()
	feats := auth.DefaultIAMFeatures()
	feats.M2MAdminRoleEnabled = flagOn
	return New(nil, nil, nil, nil, auth.NewInMemoryM2MClientStore(), feats)
}

func withTenantAdminCtx(req *http.Request, tenantID string) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID:   "admin-user",
		UserName: "Admin User",
		Tenant:   spi.Tenant{ID: spi.TenantID(tenantID), Name: tenantID},
		Roles:    []string{"ROLE_ADMIN"},
	}))
}

func withTenantNonAdminCtx(req *http.Request, tenantID string) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID:   "regular-user",
		UserName: "Regular User",
		Tenant:   spi.Tenant{ID: spi.TenantID(tenantID), Name: tenantID},
		Roles:    []string{"ROLE_USER"},
	}))
}

func decodeErrCode(t *testing.T, body []byte) string {
	t.Helper()
	// RFC 9457 problem-detail: errorCode lives under properties.errorCode.
	var env struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v\nbody: %s", err, string(body))
	}
	code, _ := env.Properties["errorCode"].(string)
	return code
}

// --- Case 1: List, admin, empty store ---

func TestListTechnicalUsers_AdminEmpty_Returns200EmptyArray(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()

	h.ListTechnicalUsers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if body != "[]" {
		t.Errorf("body: got %q want %q", body, "[]")
	}
}

// --- Case 2: List, admin, mixed-tenant store, returns caller's tenant only ---

func TestListTechnicalUsers_AdminMixedTenant_FiltersOnCallerTenant(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	store := h.m2mClientStore.(*auth.InMemoryM2MClientStore)
	if _, err := store.Create("CLIENTAONE", spi.TenantID(tenantA), "CLIENTAONE", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("seed A1: %v", err)
	}
	if _, err := store.Create("CLIENTATWO", spi.TenantID(tenantA), "CLIENTATWO", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("seed A2: %v", err)
	}
	if _, err := store.Create("CLIENTBONE", spi.TenantID(tenantB), "CLIENTBONE", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("seed B1: %v", err)
	}

	req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()
	h.ListTechnicalUsers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	var got []genapi.TechnicalUserDto
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, c := range got {
		seen[c.ClientId] = true
		if c.CreationDate.IsZero() {
			t.Errorf("client %s: CreationDate zero", c.ClientId)
		}
	}
	if !seen["CLIENTAONE"] || !seen["CLIENTATWO"] {
		t.Errorf("missing expected clients: %v", seen)
	}
	if seen["CLIENTBONE"] {
		t.Errorf("tenant B's client leaked: %v", seen)
	}
}

// --- Case 3: List, non-admin → 403 FORBIDDEN ---

func TestListTechnicalUsers_NonAdmin_Returns403Forbidden(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()

	h.ListTechnicalUsers(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeForbidden {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeForbidden)
	}
}

// --- Case 4: List, no user context → 401 UNAUTHORIZED ---

func TestListTechnicalUsers_NoUserContext_Returns401Unauthorized(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := httptest.NewRequest(http.MethodGet, "/clients", nil) // no context
	rr := httptest.NewRecorder()

	h.ListTechnicalUsers(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeUnauthorized {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeUnauthorized)
	}
}

// --- Case 5: List, nil store → 501 NOT_IMPLEMENTED ---

func TestListTechnicalUsers_NilStore_Returns501NotImplemented(t *testing.T) {
	feats := auth.DefaultIAMFeatures()
	h := New(nil, nil, nil, nil, nil, feats) // explicitly nil store

	req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()
	h.ListTechnicalUsers(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d want 501", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeNotImplemented {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeNotImplemented)
	}
}
