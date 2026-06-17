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

// oidcTenantUUID is the UUID representation of oidcTenantID. Used for
// OwnerLegalEntityID in RegisterInput.
const oidcTenantID = spi.TenantID("00000000-0000-0000-0000-00000000aabb")

var oidcTenantUUID = uuid.MustParse(string(oidcTenantID))

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
	reg := oidc.NewRegistry(store, &oidcFakeDiscovery{}, nil, oidc.NopMetrics{}, nil, true /* allowPrivate: tests bind to httptest.Server on 127.0.0.1 */)
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

// jsonBody encodes v to JSON and returns an *http.Request body reader.
func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return bytes.NewBuffer(b)
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

func TestRegisterOidcProvider_Duplicate_Returns400ProviderDuplicate(t *testing.T) {
	h := newOidcAdapterFixture(t)
	uri := "https://idp.example/.well-known/openid-configuration"

	// First register should succeed.
	registerProvider(t, h, uri)

	// Second register same URI same tenant → duplicate.
	body := rawBody(`{"wellKnownConfigUri":"` + uri + `"}`)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers", body))
	rr := httptest.NewRecorder()
	h.RegisterOidcProvider(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
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
	activeStr := "true"
	req := withOidcTenantUserCtx(httptest.NewRequest(http.MethodGet, "/oauth/oidc/providers?activeOnly=true", nil))
	rr := httptest.NewRecorder()
	h.ListOidcProviders(rr, req, genapi.ListOidcProvidersParams{ActiveOnly: &activeStr})
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
