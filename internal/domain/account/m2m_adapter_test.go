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

// --- Case 6: Create, admin, withAdminRole absent ---

func TestCreateTechnicalUser_AdminNoFlag_Returns200WithM2MRoleOnly(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()

	h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !clientIDPattern.MatchString(creds.ClientId) || len(creds.ClientId) != 16 {
		t.Errorf("clientId %q: want 16-char [A-Za-z0-9]", creds.ClientId)
	}
	if creds.ClientSecret == "" {
		t.Error("client_secret empty")
	}
	if string(creds.GrantType) != "client_credentials" {
		t.Errorf("grant_type: got %q want client_credentials", creds.GrantType)
	}
	if creds.ClientSecretExpiresAt != 0 {
		t.Errorf("client_secret_expires_at: got %d want 0", creds.ClientSecretExpiresAt)
	}

	// Store side: roles == ["ROLE_M2M"], CreatedAt == UpdatedAt, tenant == caller.
	stored, err := h.m2mClientStore.Get(creds.ClientId)
	if err != nil {
		t.Fatalf("Get(%s): %v", creds.ClientId, err)
	}
	if len(stored.Roles) != 1 || stored.Roles[0] != "ROLE_M2M" {
		t.Errorf("stored roles: got %v want [ROLE_M2M]", stored.Roles)
	}
	if stored.TenantID != spi.TenantID(tenantA) {
		t.Errorf("stored tenant: got %q want %q", stored.TenantID, tenantA)
	}
	if !stored.CreatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("CreatedAt (%v) != UpdatedAt (%v)", stored.CreatedAt, stored.UpdatedAt)
	}
}

// --- Case 7: Create, admin, withAdminRole=true, flag on ---

func TestCreateTechnicalUser_AdminWithAdminRoleFlagOn_AddsAdminRole(t *testing.T) {
	h := newM2MAdapterFixture(t, true)
	val := "true"
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=true", nil), tenantA)
	rr := httptest.NewRecorder()

	h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stored, err := h.m2mClientStore.Get(creds.ClientId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := map[string]bool{"ROLE_M2M": true, "ROLE_ADMIN": true}
	got := map[string]bool{}
	for _, r := range stored.Roles {
		got[r] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("stored roles missing %q: %v", k, stored.Roles)
		}
	}
}

// --- Case 8: Create, admin, withAdminRole=true, flag OFF ---

func TestCreateTechnicalUser_AdminWithAdminRoleFlagOff_Returns404FeatureDisabled(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	val := "true"
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=true", nil), tenantA)
	rr := httptest.NewRecorder()

	h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeFeatureDisabled {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeFeatureDisabled)
	}
	// No store record should have been created.
	if len(h.m2mClientStore.List()) != 0 {
		t.Errorf("store should remain empty on FEATURE_DISABLED, got %d records", len(h.m2mClientStore.List()))
	}
}

// --- Case 9: Create, admin, withAdminRole=false (explicit) ---

func TestCreateTechnicalUser_AdminWithAdminRoleFalse_NoAdminRole(t *testing.T) {
	h := newM2MAdapterFixture(t, true)
	val := "false"
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=false", nil), tenantA)
	rr := httptest.NewRecorder()

	h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	var creds genapi.TechnicalUserCredentialsDto
	_ = json.Unmarshal(rr.Body.Bytes(), &creds)
	stored, _ := h.m2mClientStore.Get(creds.ClientId)
	for _, r := range stored.Roles {
		if r == "ROLE_ADMIN" {
			t.Errorf("ROLE_ADMIN must NOT be present when withAdminRole=false; got %v", stored.Roles)
		}
	}
}

// --- Case 10: Create, non-admin → 403 FORBIDDEN ---

func TestCreateTechnicalUser_NonAdmin_Returns403Forbidden(t *testing.T) {
	h := newM2MAdapterFixture(t, true)
	req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()

	h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

// --- Case 11: Invalid withAdminRole value (string mode, transitional) ---

func TestCreateTechnicalUser_InvalidWithAdminRoleValue_Returns400BadRequest(t *testing.T) {
	h := newM2MAdapterFixture(t, true)
	val := "yes" // not "true"/"false"
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=yes", nil), tenantA)
	rr := httptest.NewRecorder()

	h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeBadRequest {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeBadRequest)
	}
}

// --- Case 12: Repeated creates produce 100 distinct clientIds ---

func TestCreateTechnicalUser_RepeatedCreates_NoCollisions(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	seen := map[string]bool{}
	const n = 100
	for i := 0; i < n; i++ {
		req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
		rr := httptest.NewRecorder()
		h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})
		if rr.Code != http.StatusOK {
			t.Fatalf("iter %d: status %d, body=%s", i, rr.Code, rr.Body.String())
		}
		var creds genapi.TechnicalUserCredentialsDto
		_ = json.Unmarshal(rr.Body.Bytes(), &creds)
		if seen[creds.ClientId] {
			t.Fatalf("clientId collision at iter %d: %q reused", i, creds.ClientId)
		}
		seen[creds.ClientId] = true
	}
	if len(h.m2mClientStore.List()) != n {
		t.Errorf("store size: got %d want %d (some Create silently overwrote?)", len(h.m2mClientStore.List()), n)
	}
}

// --- Case 13: Delete, admin, owned ---

func TestDeleteTechnicalUser_AdminOwned_Returns200AndRemoves(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	if _, err := h.m2mClientStore.Create("CLIENT1", spi.TenantID(tenantA), "CLIENT1", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/CLIENT1", nil), tenantA)
	rr := httptest.NewRecorder()

	h.DeleteTechnicalUser(rr, req, "CLIENT1")

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp genapi.DeleteTechnicalUser200ResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ClientId != "CLIENT1" {
		t.Errorf("clientId: got %q want CLIENT1", resp.ClientId)
	}
	if resp.Message == "" {
		t.Error("message empty")
	}
	if _, err := h.m2mClientStore.Get("CLIENT1"); err == nil {
		t.Error("store record still present after Delete")
	}
}

// --- Case 14: Delete, admin, cross-tenant target ---

func TestDeleteTechnicalUser_AdminCrossTenant_Returns404AndPreservesRecord(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	if _, err := h.m2mClientStore.Create("CLIENTB", spi.TenantID(tenantB), "CLIENTB", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/CLIENTB", nil), tenantA)
	rr := httptest.NewRecorder()

	h.DeleteTechnicalUser(rr, req, "CLIENTB")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeM2MClientNotFound {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeM2MClientNotFound)
	}
	// Tenant B's record must remain untouched.
	if _, err := h.m2mClientStore.Get("CLIENTB"); err != nil {
		t.Errorf("tenant B record removed by cross-tenant DELETE: %v", err)
	}
}

// --- Case 15: Delete, admin, unknown id ---

func TestDeleteTechnicalUser_AdminUnknown_Returns404(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/UNKNOWN", nil), tenantA)
	rr := httptest.NewRecorder()

	h.DeleteTechnicalUser(rr, req, "UNKNOWN")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeM2MClientNotFound {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeM2MClientNotFound)
	}
}

// --- Case 16: Delete, admin, malformed id (hyphen) ---

func TestDeleteTechnicalUser_AdminMalformedId_Returns400(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/bad-id", nil), tenantA)
	rr := httptest.NewRecorder()

	h.DeleteTechnicalUser(rr, req, "bad-id")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeBadRequest {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeBadRequest)
	}
}

// --- Case 17: Delete, admin, empty id (trailing slash) ---

func TestDeleteTechnicalUser_AdminEmptyId_Returns400(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/", nil), tenantA)
	rr := httptest.NewRecorder()

	h.DeleteTechnicalUser(rr, req, "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}

// --- Case 18: Delete, non-admin → 403 ---

func TestDeleteTechnicalUser_NonAdmin_Returns403(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	if _, err := h.m2mClientStore.Create("CLIENT1", spi.TenantID(tenantA), "CLIENT1", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/CLIENT1", nil), tenantA)
	rr := httptest.NewRecorder()

	h.DeleteTechnicalUser(rr, req, "CLIENT1")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

// --- Case 19: Reset, admin, owned ---

func TestResetTechnicalUserSecret_AdminOwned_Returns200AndRotatesSecret(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	plaintextOld, err := h.m2mClientStore.Create("CLIENTR", spi.TenantID(tenantA), "CLIENTR", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/CLIENTR/secret", nil), tenantA)
	rr := httptest.NewRecorder()

	h.ResetTechnicalUserSecret(rr, req, "CLIENTR")

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if creds.ClientId != "CLIENTR" {
		t.Errorf("client_id: got %q want CLIENTR", creds.ClientId)
	}
	if creds.ClientSecret == plaintextOld {
		t.Error("new client_secret equals old (reset did not rotate)")
	}
	// VerifySecret accepts the new and rejects the old.
	okNew, _ := h.m2mClientStore.VerifySecret("CLIENTR", creds.ClientSecret)
	if !okNew {
		t.Error("new secret does not verify against store")
	}
	okOld, _ := h.m2mClientStore.VerifySecret("CLIENTR", plaintextOld)
	if okOld {
		t.Error("old secret still verifies after reset")
	}
	// UpdatedAt advanced.
	stored, _ := h.m2mClientStore.Get("CLIENTR")
	if !stored.UpdatedAt.After(stored.CreatedAt) {
		t.Errorf("UpdatedAt did not advance past CreatedAt (created=%v updated=%v)", stored.CreatedAt, stored.UpdatedAt)
	}
}

// --- Case 20: Reset, cross-tenant ---

func TestResetTechnicalUserSecret_AdminCrossTenant_Returns404AndPreservesSecret(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	plaintextB, _ := h.m2mClientStore.Create("CLIENTB", spi.TenantID(tenantB), "CLIENTB", []string{"ROLE_M2M"})

	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/CLIENTB/secret", nil), tenantA)
	rr := httptest.NewRecorder()

	h.ResetTechnicalUserSecret(rr, req, "CLIENTB")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeM2MClientNotFound {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeM2MClientNotFound)
	}
	okB, _ := h.m2mClientStore.VerifySecret("CLIENTB", plaintextB)
	if !okB {
		t.Error("tenant B's secret was rotated by cross-tenant reset")
	}
}

// --- Case 21: Reset, unknown id ---

func TestResetTechnicalUserSecret_AdminUnknown_Returns404(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/UNKNOWN/secret", nil), tenantA)
	rr := httptest.NewRecorder()
	h.ResetTechnicalUserSecret(rr, req, "UNKNOWN")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
}

// --- Case 22: Reset, malformed id ---

func TestResetTechnicalUserSecret_AdminMalformedId_Returns400(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/bad-id/secret", nil), tenantA)
	rr := httptest.NewRecorder()
	h.ResetTechnicalUserSecret(rr, req, "bad-id")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}

// --- Case 23: Reset, empty id ---

func TestResetTechnicalUserSecret_AdminEmptyId_Returns400(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients//secret", nil), tenantA)
	rr := httptest.NewRecorder()
	h.ResetTechnicalUserSecret(rr, req, "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}

// --- Case 24: Reset, non-admin → 403 ---

func TestResetTechnicalUserSecret_NonAdmin_Returns403(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	_, _ = h.m2mClientStore.Create("CLIENT1", spi.TenantID(tenantA), "CLIENT1", []string{"ROLE_M2M"})

	req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/CLIENT1/secret", nil), tenantA)
	rr := httptest.NewRecorder()
	h.ResetTechnicalUserSecret(rr, req, "CLIENT1")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}
