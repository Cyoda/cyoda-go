package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/auth/oidc"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	memplugin "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// ---------- fixture helpers ----------

// oidcTenantID is the UUID-shaped tenant every test in this file registers
// providers as, unless it is specifically varying the tenant.
const oidcTenantID = spi.TenantID("00000000-0000-0000-0000-00000000aabb")

// newOidcTestService builds an oidc.Service over an in-memory KV store with a
// fake discovery (no network) so adapter tests stay offline.
func newOidcTestService(t *testing.T) *oidc.Service {
	t.Helper()
	factory := memplugin.NewStoreFactory()
	systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "system",
		UserName: "System",
		Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
	})
	kv, err := factory.KeyValueStore(systemCtx)
	if err != nil {
		t.Fatalf("memory KV: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })

	store, err := oidc.NewKVProviderStore(systemCtx, kv)
	if err != nil {
		t.Fatalf("NewKVProviderStore: %v", err)
	}
	reg := oidc.NewRegistry(store, &oidcFakeDiscovery{}, nil, oidc.NopMetrics{}, nil,
		oidc.RegistryConfig{AllowPrivateNetworks: true}) // tests bind to httptest.Server on 127.0.0.1
	return oidc.NewService(store, reg, nil)
}

// oidcFakeDiscovery never fetches over the network.
type oidcFakeDiscovery struct{}

func (d *oidcFakeDiscovery) Fetch(_ context.Context, _ string) (*oidc.DiscoveryDoc, error) {
	return &oidc.DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"}, nil
}

// newOidcAdapterFixture returns a pre-wired *Handler with an oidcAdapter.
// requireHTTPS=false, allowPrivate=true to test-friendly defaults.
func newOidcAdapterFixture(t *testing.T) *Handler {
	t.Helper()
	svc := newOidcTestService(t)
	a := &OidcAdapter{adapter: newOidcAdapter(svc, "roles", false, true)}
	h := New(nil, nil, nil, nil, nil, defaultFeatures())
	h.WithOIDCAdapter(a)
	return h
}

func defaultFeatures() auth.IAMFeatures { return auth.DefaultIAMFeatures() }

// withOidcTenantAdminCtx puts oidcTenantID admin user in the request context.
func withOidcTenantAdminCtx(req *http.Request) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID:   "admin-user",
		UserName: "Admin User",
		Tenant:   spi.Tenant{ID: oidcTenantID, Name: "test-tenant"},
		Roles:    []string{"ROLE_ADMIN"},
	}))
}

// withOidcTenantUserCtx puts oidcTenantID non-admin user in the request context.
func withOidcTenantUserCtx(req *http.Request) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID:   "regular-user",
		UserName: "Regular User",
		Tenant:   spi.Tenant{ID: oidcTenantID, Name: "test-tenant"},
		Roles:    []string{"ROLE_USER"},
	}))
}

// rawBody returns a buffer from a raw JSON string (for tri-state tests).
func rawBody(s string) *bytes.Buffer { return bytes.NewBufferString(s) }

// registerProvider is a test helper that registers a provider and returns its DTO.
// Fails the test on any error.
func registerProvider(t *testing.T, h *Handler, uri string) genapi.OidcProviderResponseDto {
	t.Helper()
	body := rawBody(`{"wellKnownConfigUri":"` + uri + `","issuers":["https://idp.example"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()
	h.RegisterOidcProvider(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Register: status %d, body=%s", rr.Code, rr.Body.String())
	}
	var dto genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("Register decode: %v", err)
	}
	return dto
}

// ---------- D24 tri-state decoder tests: decodeOptionalStringSlice ----------

func TestDecodeOptionalStringSlice_Absent(t *testing.T) {
	raw := map[string]json.RawMessage{"other": json.RawMessage(`"hello"`)}
	got, present, err := decodeOptionalStringSlice(raw, "issuers")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if present {
		t.Error("present: got true, want false (field absent)")
	}
	if got != nil {
		t.Errorf("value: got %v, want nil", got)
	}
}

func TestDecodeOptionalStringSlice_Null(t *testing.T) {
	raw := map[string]json.RawMessage{"issuers": json.RawMessage(`null`)}
	got, present, err := decodeOptionalStringSlice(raw, "issuers")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !present {
		t.Error("present: got false, want true (field was null)")
	}
	if got != nil {
		t.Errorf("value: got %v, want nil (null means clear)", got)
	}
}

func TestDecodeOptionalStringSlice_EmptyArray(t *testing.T) {
	raw := map[string]json.RawMessage{"issuers": json.RawMessage(`[]`)}
	got, present, err := decodeOptionalStringSlice(raw, "issuers")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !present {
		t.Error("present: got false, want true (empty array present)")
	}
	if got != nil {
		t.Errorf("value: got %v, want nil (empty array treated as clear)", got)
	}
}

func TestDecodeOptionalStringSlice_Value(t *testing.T) {
	raw := map[string]json.RawMessage{"issuers": json.RawMessage(`["https://a.example","https://b.example"]`)}
	got, present, err := decodeOptionalStringSlice(raw, "issuers")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !present {
		t.Error("present: got false, want true")
	}
	if len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Errorf("value: got %v, want [https://a.example https://b.example]", got)
	}
}

// ---------- D24 tri-state decoder tests: decodeOptionalString ----------

func TestDecodeOptionalString_Absent(t *testing.T) {
	raw := map[string]json.RawMessage{"other": json.RawMessage(`"x"`)}
	got, present, err := decodeOptionalString(raw, "rolesClaim")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if present {
		t.Error("present: got true, want false (field absent)")
	}
	if got != nil {
		t.Errorf("value: got %v, want nil", got)
	}
}

func TestDecodeOptionalString_Null(t *testing.T) {
	raw := map[string]json.RawMessage{"rolesClaim": json.RawMessage(`null`)}
	got, present, err := decodeOptionalString(raw, "rolesClaim")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !present {
		t.Error("present: got false, want true (field was null)")
	}
	if got != nil {
		t.Errorf("value: got %v, want nil (null resets claim)", got)
	}
}

func TestDecodeOptionalString_EmptyString(t *testing.T) {
	// An explicit empty string is treated as a value (present=true, value="").
	raw := map[string]json.RawMessage{"rolesClaim": json.RawMessage(`""`)}
	got, present, err := decodeOptionalString(raw, "rolesClaim")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !present {
		t.Error("present: got false, want true")
	}
	if got == nil || *got != "" {
		t.Errorf("value: got %v, want pointer to empty string", got)
	}
}

func TestDecodeOptionalString_Value(t *testing.T) {
	raw := map[string]json.RawMessage{"rolesClaim": json.RawMessage(`"custom_roles"`)}
	got, present, err := decodeOptionalString(raw, "rolesClaim")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !present {
		t.Error("present: got false, want true")
	}
	if got == nil || *got != "custom_roles" {
		t.Errorf("value: got %v, want %q", got, "custom_roles")
	}
}

// ---------- RegisterOidcProvider handler tests ----------

func TestRegisterOidcProvider_AdminHappyPath_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	body := rawBody(`{"wellKnownConfigUri":"https://idp.example/.well-known/openid-configuration","issuers":["https://idp.example"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var dto genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Id == (uuid.UUID{}) {
		t.Error("id is zero UUID")
	}
	if dto.WellKnownConfigUri != "https://idp.example/.well-known/openid-configuration" {
		t.Errorf("wellKnownConfigUri: got %q", dto.WellKnownConfigUri)
	}
	if !dto.Active {
		t.Error("newly registered provider should be active")
	}
}

func TestRegisterOidcProvider_NonAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	body := rawBody(`{"wellKnownConfigUri":"https://idp.example/.well-known/openid-configuration"}`)
	req := withOidcTenantUserCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeForbidden {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeForbidden)
	}
}

func TestRegisterOidcProvider_Duplicate_Returns409ProviderDuplicate(t *testing.T) {
	h := newOidcAdapterFixture(t)
	uri := "https://idp.example/.well-known/openid-configuration"

	// First register should succeed.
	registerProvider(t, h, uri)

	// Second register same URI same tenant → duplicate → 409 Conflict.
	body := rawBody(`{"wellKnownConfigUri":"` + uri + `"}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()
	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOIDCProviderDuplicate {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOIDCProviderDuplicate)
	}
}

func TestRegisterOidcProvider_MissingURI_Returns400(t *testing.T) {
	h := newOidcAdapterFixture(t)
	body := rawBody(`{"issuers":["https://idp.example"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}

func TestRegisterOidcProvider_NoContext_Returns401(t *testing.T) {
	h := newOidcAdapterFixture(t)
	body := rawBody(`{"wellKnownConfigUri":"https://idp.example/.well-known/openid-configuration"}`)
	req := httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body)
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
}

func TestRegisterOidcProvider_IssuersExceedMax_Returns400(t *testing.T) {
	h := newOidcAdapterFixture(t)
	// Build 11 issuers (> max 10).
	issuers := make([]string, 11)
	for i := range issuers {
		issuers[i] = "https://idp.example"
	}
	b, _ := json.Marshal(map[string]any{
		"wellKnownConfigUri": "https://idp.example/.well-known/openid-configuration",
		"issuers":            issuers,
	})
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", bytes.NewBuffer(b)))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegisterOidcProvider_RejectsEmptyIssuerInList(t *testing.T) {
	h := newOidcAdapterFixture(t)
	// issuers contains one valid entry and one empty string — must reject.
	b, _ := json.Marshal(map[string]any{
		"wellKnownConfigUri": "https://idp.example/.well-known/openid-configuration",
		"issuers":            []string{"https://idp.example", ""},
	})
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", bytes.NewBuffer(b)))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (empty issuer), body=%s", rr.Code, rr.Body.String())
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeBadRequest {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeBadRequest)
	}
}

// ---------- ListOidcProviders handler tests ----------

func TestListOidcProviders_AuthenticatedMember_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	// Seed a provider.
	registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	// Non-admin (any tenant member) can list.
	req := withOidcTenantUserCtx(httptest.NewRequest(http.MethodGet, "/oauth/oidc/providers", nil))
	rr := httptest.NewRecorder()
	h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{})

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var providers []genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &providers); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(providers) != 1 {
		t.Errorf("count: got %d want 1", len(providers))
	}
}

func TestListOidcProviders_NoContext_Returns401(t *testing.T) {
	h := newOidcAdapterFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/oauth/oidc/providers", nil) // no user context
	rr := httptest.NewRecorder()

	h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{})

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeUnauthorized {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeUnauthorized)
	}
}

func TestListOidcProviders_ActiveOnlyFilter_ExcludesInvalidated(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	// Invalidate the provider.
	invalidateReq := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/invalidate", nil))
	invalidateRec := httptest.NewRecorder()
	h.InvalidateOidcProvider(invalidateRec, invalidateReq, dto.Id)
	if invalidateRec.Code != http.StatusOK {
		t.Fatalf("invalidate: status %d", invalidateRec.Code)
	}

	// activeOnly=true → should return 0 providers.
	activeBool := true
	req := withOidcTenantUserCtx(httptest.NewRequest(http.MethodGet, "/oauth/oidc/providers?activeOnly=true", nil))
	rr := httptest.NewRecorder()
	h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{ActiveOnly: &activeBool})
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d", rr.Code)
	}
	var providers []genapi.OidcProviderResponseDto
	_ = json.Unmarshal(rr.Body.Bytes(), &providers)
	if len(providers) != 0 {
		t.Errorf("activeOnly=true: got %d providers, want 0", len(providers))
	}
}

// ---------- UpdateOidcProvider handler tests ----------

func TestUpdateOidcProvider_AdminHappyPath_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	body := rawBody(`{"issuers":["https://new-issuer.example"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPatch, "/oauth/oidc/providers/"+dto.Id.String(), body))
	rr := httptest.NewRecorder()

	h.UpdateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var updated genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Issuers == nil || len(*updated.Issuers) != 1 || (*updated.Issuers)[0] != "https://new-issuer.example" {
		t.Errorf("issuers: got %v", updated.Issuers)
	}
}

func TestUpdateOidcProvider_NonAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	body := rawBody(`{"issuers":["https://x.example"]}`)
	req := withOidcTenantUserCtx(httptest.NewRequest(http.MethodPatch, "/oauth/oidc/providers/"+dto.Id.String(), body))
	rr := httptest.NewRecorder()

	h.UpdateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

func TestUpdateOidcProvider_NotFound_Returns404(t *testing.T) {
	h := newOidcAdapterFixture(t)
	nonExistent := openapi_types_UUID(t, uuid.New().String())
	body := rawBody(`{"issuers":["https://x.example"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPatch, "/oauth/oidc/providers/"+nonExistent.String(), body))
	rr := httptest.NewRecorder()

	h.UpdateOidcProvider(rr, req, nonExistent)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOIDCProviderNotFound {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOIDCProviderNotFound)
	}
}

func TestUpdateOidcProvider_Inactive_Returns409(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	// Invalidate first.
	invalidateReq := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/invalidate", nil))
	invalidateRec := httptest.NewRecorder()
	h.InvalidateOidcProvider(invalidateRec, invalidateReq, dto.Id)
	if invalidateRec.Code != http.StatusOK {
		t.Fatalf("invalidate: status %d", invalidateRec.Code)
	}

	// Update on inactive → 409.
	body := rawBody(`{"issuers":["https://x.example"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPatch, "/oauth/oidc/providers/"+dto.Id.String(), body))
	rr := httptest.NewRecorder()
	h.UpdateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOIDCProviderInactive {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOIDCProviderInactive)
	}
}

func TestUpdateOidcProvider_TriState_IssuersAbsent_LeaveUnchanged(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	// PATCH with no issuers field → issuers unchanged.
	body := rawBody(`{"expectedAudiences":["aud1"]}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPatch, "/oauth/oidc/providers/"+dto.Id.String(), body))
	rr := httptest.NewRecorder()
	h.UpdateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	// Verify original issuers still present.
	var updated genapi.OidcProviderResponseDto
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Issuers == nil || len(*updated.Issuers) == 0 {
		t.Error("issuers were cleared despite being absent from PATCH body")
	}
}

// ---------- InvalidateOidcProvider handler tests ----------

func TestInvalidateOidcProvider_AdminHappyPath_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	req := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/invalidate", nil))
	rr := httptest.NewRecorder()

	h.InvalidateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
}

func TestInvalidateOidcProvider_NonAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	req := withOidcTenantUserCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/invalidate", nil))
	rr := httptest.NewRecorder()

	h.InvalidateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

func TestInvalidateOidcProvider_NotFound_Returns404(t *testing.T) {
	h := newOidcAdapterFixture(t)
	nonExistent := openapi_types_UUID(t, uuid.New().String())
	req := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+nonExistent.String()+"/invalidate", nil))
	rr := httptest.NewRecorder()

	h.InvalidateOidcProvider(rr, req, nonExistent)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOIDCProviderNotFound {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOIDCProviderNotFound)
	}
}

// ---------- ReactivateOidcProvider handler tests ----------

func TestReactivateOidcProvider_AdminHappyPath_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	// Invalidate first.
	invalidateReq := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/invalidate", nil))
	invalidateRec := httptest.NewRecorder()
	h.InvalidateOidcProvider(invalidateRec, invalidateReq, dto.Id)

	// Reactivate.
	body := rawBody(`{"reactivateKeys":true}`)
	req := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/reactivate", body))
	rr := httptest.NewRecorder()

	h.ReactivateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	var reactivated genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &reactivated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reactivated.Active {
		t.Error("reactivated provider should be active")
	}
}

func TestReactivateOidcProvider_NonAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	req := withOidcTenantUserCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+dto.Id.String()+"/reactivate", nil))
	rr := httptest.NewRecorder()

	h.ReactivateOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

func TestReactivateOidcProvider_NotFound_Returns404(t *testing.T) {
	h := newOidcAdapterFixture(t)
	nonExistent := openapi_types_UUID(t, uuid.New().String())
	req := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/"+nonExistent.String()+"/reactivate", nil))
	rr := httptest.NewRecorder()

	h.ReactivateOidcProvider(rr, req, nonExistent)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
}

// ---------- DeleteOidcProvider handler tests ----------

func TestDeleteOidcProvider_AdminHappyPath_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	req := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodDelete, "/oauth/oidc/providers/"+dto.Id.String(), nil))
	rr := httptest.NewRecorder()

	h.DeleteOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
}

func TestDeleteOidcProvider_NonAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	dto := registerProvider(t, h, "https://idp.example/.well-known/openid-configuration")

	req := withOidcTenantUserCtx(
		httptest.NewRequest(http.MethodDelete, "/oauth/oidc/providers/"+dto.Id.String(), nil))
	rr := httptest.NewRecorder()

	h.DeleteOidcProvider(rr, req, dto.Id)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

func TestDeleteOidcProvider_NotFound_Returns404(t *testing.T) {
	h := newOidcAdapterFixture(t)
	nonExistent := openapi_types_UUID(t, uuid.New().String())
	req := withOidcTenantAdminCtx(
		httptest.NewRequest(http.MethodDelete, "/oauth/oidc/providers/"+nonExistent.String(), nil))
	rr := httptest.NewRecorder()

	h.DeleteOidcProvider(rr, req, nonExistent)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOIDCProviderNotFound {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOIDCProviderNotFound)
	}
}

// ---------- ReloadOidcProviders handler tests ----------

func TestReloadOidcProviders_AdminHappyPath_Returns200(t *testing.T) {
	h := newOidcAdapterFixture(t)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/reload", nil))
	rr := httptest.NewRecorder()

	h.ReloadOidcProviders(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
}

func TestReloadOidcProviders_NonAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	req := withOidcTenantUserCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/reload", nil))
	rr := httptest.NewRecorder()

	h.ReloadOidcProviders(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rr.Code)
	}
}

func TestReloadOidcProviders_NoContext_Returns401(t *testing.T) {
	h := newOidcAdapterFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/reload", nil)
	rr := httptest.NewRecorder()

	h.ReloadOidcProviders(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rr.Code)
	}
}

// ---------- Unimplemented stub path ----------

func TestHandler_OidcStub_Returns501_WhenAdapterNil(t *testing.T) {
	// Handler with no oidc adapter installed → falls back to stub → 501.
	feats := auth.DefaultIAMFeatures()
	h := New(nil, nil, nil, nil, nil, feats) // no WithOIDCAdapter

	cases := []struct {
		name string
		do   func(rr *httptest.ResponseRecorder, req *http.Request)
	}{
		{
			name: "RegisterOidcProvider",
			do:   func(rr *httptest.ResponseRecorder, req *http.Request) { h.RegisterOidcProvider(rr, req) },
		},
		{
			name: "ReloadOidcProviders",
			do:   func(rr *httptest.ResponseRecorder, req *http.Request) { h.ReloadOidcProviders(rr, req) },
		},
		{
			name: "ListOidcProviders",
			do: func(rr *httptest.ResponseRecorder, req *http.Request) {
				h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/", nil))
			rr := httptest.NewRecorder()
			tc.do(rr, req)
			if rr.Code != http.StatusNotImplemented {
				t.Errorf("%s: got %d want 501", tc.name, rr.Code)
			}
		})
	}
}

// ---------- Non-UUID tenant rejection (Critical-2 fix) ----------

// withNonUUIDTenantAdminCtx puts a non-UUID tenant admin user in the request
// context. This mimics a bootstrap deployment where CYODA_BOOTSTRAP_TENANT_ID
// is set to the literal "default-tenant" string.
func withNonUUIDTenantAdminCtx(req *http.Request) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID:   "admin-user",
		UserName: "Admin User",
		Tenant:   spi.Tenant{ID: "default-tenant", Name: "default-tenant"},
		Roles:    []string{"ROLE_ADMIN"},
	}))
}

// TestOidcAdapter_NonUUIDTenantRejected verifies that RegisterOidcProvider
// returns 400 with code OIDC_INVALID_TENANT (not silently coercing to uuid.Nil)
// when the calling tenant's ID is not a valid UUID.
//
// Background: OwnerLegalEntityID is a uuid.UUID. Coercing a non-UUID tenant to
// uuid.Nil would collide in KV storage across all non-UUID tenants and produce
// a synthetic "nil tenant" identity at token validation time.
func TestOidcAdapter_NonUUIDTenantRejected(t *testing.T) {
	h := newOidcAdapterFixture(t)
	body := rawBody(`{"wellKnownConfigUri":"https://idp.example/.well-known/openid-configuration"}`)
	req := withNonUUIDTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (non-UUID tenant must be rejected), body=%s", rr.Code, rr.Body.String())
	}
	if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOidcInvalidTenant {
		t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOidcInvalidTenant)
	}
}

// ---------- Content-Type hygiene ----------

func TestRegisterOidcProvider_ResponseContentType_IsApplicationJSON(t *testing.T) {
	h := newOidcAdapterFixture(t)
	body := rawBody(`{"wellKnownConfigUri":"https://idp.example/.well-known/openid-configuration"}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()

	h.RegisterOidcProvider(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q want application/json prefix", ct)
	}
}

// ---------- helpers ----------

// openapi_types_UUID converts a UUID string to openapi_types.UUID.
func openapi_types_UUID(t *testing.T, s string) openapi_types.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse UUID %q: %v", s, err)
	}
	return u
}

// ---------- Tenant identity, not tenant spelling ----------

// withOidcAdminCtxForTenant is withOidcTenantAdminCtx with the tenant under the
// caller's control. The hardcoded helper covers every other test in this file;
// varying the tenant's spelling is the one thing it cannot do.
func withOidcAdminCtxForTenant(req *http.Request, tenant string) *http.Request {
	return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
		UserID:   "admin-user",
		UserName: "Admin User",
		Tenant:   spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
		Roles:    []string{"ROLE_ADMIN"},
	}))
}

// registerProviderAs registers uri as tenant and returns the raw recorder so
// the caller can assert on a non-200 as well.
func registerProviderAs(t *testing.T, h *Handler, tenant, uri string) *httptest.ResponseRecorder {
	t.Helper()
	body := rawBody(`{"wellKnownConfigUri":"` + uri + `"}`)
	req := withOidcAdminCtxForTenant(
		httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body), tenant)
	rr := httptest.NewRecorder()
	h.RegisterOidcProvider(rr, req)
	return rr
}

// listProvidersAs lists as tenant and returns the status plus the decoded body
// (nil on a non-200).
func listProvidersAs(t *testing.T, h *Handler, tenant string) (int, []genapi.OidcProviderResponseDto) {
	t.Helper()
	req := withOidcAdminCtxForTenant(
		httptest.NewRequest(http.MethodGet, "/oauth/oidc/providers", nil), tenant)
	rr := httptest.NewRecorder()
	h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{})
	if rr.Code != http.StatusOK {
		return rr.Code, nil
	}
	var out []genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("list decode: %v; body=%s", err, rr.Body.String())
	}
	return rr.Code, out
}

// TestOidcAdapter_CanonicalUUIDTenantRoundTrips is the regression for the
// provider store keying a write and a read differently.
//
// Register keyed the provider blob and the URI index by
// OwnerLegalEntityID.String() - always canonical lowercase - while Get,
// GetByURI, Delete, ListByTenant and RaceValidateIndex keyed by the caller's
// raw tenant. The two must address the same key, so every operation in the
// lifecycle below has to reach the record registration wrote.
func TestOidcAdapter_CanonicalUUIDTenantRoundTrips(t *testing.T) {
	const tenant = "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"
	const uri = "https://idp.example/.well-known/openid-configuration"

	h := newOidcAdapterFixture(t)

	rr := registerProviderAs(t, h, tenant, uri)
	if rr.Code != http.StatusOK {
		t.Fatalf("register: status %d, body=%s", rr.Code, rr.Body.String())
	}
	var created genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("register decode: %v; body=%s", err, rr.Body.String())
	}

	// List must find what Register wrote.
	code, listed := listProvidersAs(t, h, tenant)
	if code != http.StatusOK {
		t.Fatalf("list: status %d, want 200", code)
	}
	if len(listed) != 1 {
		t.Fatalf("list returned %d providers, want 1 - the blob is stranded under a key this tenant cannot address", len(listed))
	}
	if listed[0].Id != created.Id {
		t.Errorf("list returned provider %s, want %s", listed[0].Id, created.Id)
	}

	// Every id-addressed lifecycle op must reach the same blob.
	updReq := withOidcAdminCtxForTenant(httptest.NewRequest(http.MethodPatch,
		"/oauth/oidc/providers/"+created.Id.String(),
		rawBody(`{"issuers":["https://idp.example"]}`)), tenant)
	updRec := httptest.NewRecorder()
	h.UpdateOidcProvider(updRec, updReq, created.Id)
	if updRec.Code != http.StatusOK {
		t.Fatalf("update: status %d, want 200, body=%s", updRec.Code, updRec.Body.String())
	}

	invReq := withOidcAdminCtxForTenant(httptest.NewRequest(http.MethodPost,
		"/oauth/oidc/providers/"+created.Id.String()+"/invalidate", nil), tenant)
	invRec := httptest.NewRecorder()
	h.InvalidateOidcProvider(invRec, invReq, created.Id)
	if invRec.Code != http.StatusOK {
		t.Fatalf("invalidate: status %d, want 200, body=%s", invRec.Code, invRec.Body.String())
	}

	reReq := withOidcAdminCtxForTenant(httptest.NewRequest(http.MethodPost,
		"/oauth/oidc/providers/"+created.Id.String()+"/reactivate", nil), tenant)
	reRec := httptest.NewRecorder()
	h.ReactivateOidcProvider(reRec, reReq, created.Id)
	if reRec.Code != http.StatusOK {
		t.Fatalf("reactivate: status %d, want 200, body=%s", reRec.Code, reRec.Body.String())
	}

	delReq := withOidcAdminCtxForTenant(httptest.NewRequest(http.MethodDelete,
		"/oauth/oidc/providers/"+created.Id.String(), nil), tenant)
	delRec := httptest.NewRecorder()
	h.DeleteOidcProvider(delRec, delReq, created.Id)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, want 200, body=%s", delRec.Code, delRec.Body.String())
	}

	code, remaining := listProvidersAs(t, h, tenant)
	if code != http.StatusOK || len(remaining) != 0 {
		t.Fatalf("list after delete: status %d with %d providers, want 200 with 0", code, len(remaining))
	}
}

// TestOidcAdapter_NonCanonicalUUIDTenantRejected pins the other half of the
// keying rule: the canonical spelling is *required*, never normalised to.
//
// uuid.Parse accepts spellings String() folds away - upper case, the
// hyphenless 32-hex form, braced and urn:uuid: prefixed. Everywhere else in
// the product a tenant is compared as raw text, so two UUID-equal but
// differently spelled tenants own separate entities, KV namespaces, audit
// records and messages. Folding them together on this surface alone would let
// one read, modify and delete the other's providers and register a provider
// owned by the other, which is an authentication trust anchor for a tenant the
// caller is not. Each spelling is answered with the same diagnosable 400 a
// non-UUID tenant gets.
//
// Only the upper-case and hyphenless forms can reach the adapter over HTTP -
// common.ValidateTenantID rejects the braced and urn: forms at the auth
// boundary, on `{` and `:` respectively. They are exercised anyway because the
// adapter's contract is with uuid.Parse, not with the grammar in front of it:
// the two must not be allowed to drift apart silently.
func TestOidcAdapter_NonCanonicalUUIDTenantRejected(t *testing.T) {
	const canonical = "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"
	const uri = "https://idp.example/.well-known/openid-configuration"

	for name, tenant := range map[string]string{
		"uppercase":  strings.ToUpper(canonical),
		"hyphenless": strings.ReplaceAll(canonical, "-", ""),
		"braced":     "{" + canonical + "}",
		"urn":        "urn:uuid:" + canonical,
	} {
		t.Run(name, func(t *testing.T) {
			h := newOidcAdapterFixture(t)

			rr := registerProviderAs(t, h, tenant, uri)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("register: status %d want 400 OIDC_INVALID_TENANT, body=%s", rr.Code, rr.Body.String())
			}
			if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOidcInvalidTenant {
				t.Errorf("register errorCode: got %q want %q", code, common.ErrCodeOidcInvalidTenant)
			}

			if code, _ := listProvidersAs(t, h, tenant); code != http.StatusBadRequest {
				t.Errorf("list: status %d want 400", code)
			}
		})
	}
}

// TestOidcAdapter_UUIDEqualTenantsCannotReachEachOther is the cross-tenant
// case the spelling rule exists for: two tenants whose ids parse to the same
// UUID but are spelled differently are distinct tenants everywhere in the
// product, and the OIDC surface must not be the one place they are the same.
//
// The victim's provider must stay listed, active and undeleted, and the
// UUID-equal tenant must not be able to register a provider whose owner is the
// victim's UUID - such a provider is an authentication trust anchor for the
// victim.
func TestOidcAdapter_UUIDEqualTenantsCannotReachEachOther(t *testing.T) {
	const victim = "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"
	other := strings.ToUpper(victim)
	const uri = "https://victim-idp.example/.well-known/openid-configuration"

	h := newOidcAdapterFixture(t)

	rr := registerProviderAs(t, h, victim, uri)
	if rr.Code != http.StatusOK {
		t.Fatalf("victim register: status %d, body=%s", rr.Code, rr.Body.String())
	}
	var created genapi.OidcProviderResponseDto
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("victim register decode: %v; body=%s", err, rr.Body.String())
	}

	for name, call := range map[string]func(*httptest.ResponseRecorder, *http.Request){
		"list": func(rec *httptest.ResponseRecorder, req *http.Request) {
			h.ListOidcProviders(rec, req, genapi.ListOidcProvidersParams{})
		},
		"update": func(rec *httptest.ResponseRecorder, req *http.Request) {
			h.UpdateOidcProvider(rec, req, created.Id)
		},
		"invalidate": func(rec *httptest.ResponseRecorder, req *http.Request) {
			h.InvalidateOidcProvider(rec, req, created.Id)
		},
		"reactivate": func(rec *httptest.ResponseRecorder, req *http.Request) {
			h.ReactivateOidcProvider(rec, req, created.Id)
		},
		"delete": func(rec *httptest.ResponseRecorder, req *http.Request) {
			h.DeleteOidcProvider(rec, req, created.Id)
		},
		"register": func(rec *httptest.ResponseRecorder, req *http.Request) {
			h.RegisterOidcProvider(rec, req)
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := withOidcAdminCtxForTenant(httptest.NewRequest(http.MethodPost,
				"/oauth/oidc/providers",
				rawBody(`{"wellKnownConfigUri":"https://other-idp.example/.well-known/openid-configuration"}`)),
				other)
			rec := httptest.NewRecorder()

			call(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s as the UUID-equal tenant: status %d want 400 OIDC_INVALID_TENANT, body=%s",
					name, rec.Code, rec.Body.String())
			}
			if code := decodeErrCode(t, rec.Body.Bytes()); code != common.ErrCodeOidcInvalidTenant {
				t.Errorf("%s errorCode: got %q want %q", name, code, common.ErrCodeOidcInvalidTenant)
			}
		})
	}

	// The victim's provider survived every attempt, still active.
	code, listed := listProvidersAs(t, h, victim)
	if code != http.StatusOK {
		t.Fatalf("victim list: status %d want 200", code)
	}
	if len(listed) != 1 || listed[0].Id != created.Id {
		t.Fatalf("victim list: got %d providers, want exactly its own %s", len(listed), created.Id)
	}
	if !listed[0].Active {
		t.Error("victim provider is no longer active - the UUID-equal tenant reached it")
	}
}

// TestOidcAdapter_NonUUIDTenantIsRejectedEverywhere records the behaviour
// change that ships with the keying fix. A non-UUID tenant such as
// default-tenant used to receive an empty 200 from the list endpoint, because
// its prefix scan matched nothing — a success implying a registration that
// could never have happened. Every OIDC operation now gives it the same 400
// registration always gave it.
func TestOidcAdapter_NonUUIDTenantIsRejectedEverywhere(t *testing.T) {
	const tenant = "default-tenant"
	id := uuid.New()

	for name, call := range map[string]func(*Handler, *httptest.ResponseRecorder, *http.Request){
		"list": func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) {
			h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{})
		},
		"update": func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) {
			h.UpdateOidcProvider(rr, req, id)
		},
		"invalidate": func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) {
			h.InvalidateOidcProvider(rr, req, id)
		},
		"reactivate": func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) {
			h.ReactivateOidcProvider(rr, req, id)
		},
		"delete": func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) {
			h.DeleteOidcProvider(rr, req, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newOidcAdapterFixture(t)
			// A body every op will accept; the ops that take none ignore it.
			req := withOidcAdminCtxForTenant(
				httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", rawBody(`{}`)), tenant)
			rr := httptest.NewRecorder()

			call(h, rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400 OIDC_INVALID_TENANT, body=%s", rr.Code, rr.Body.String())
			}
			if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeOidcInvalidTenant {
				t.Errorf("errorCode: got %q want %q", code, common.ErrCodeOidcInvalidTenant)
			}
		})
	}
}
