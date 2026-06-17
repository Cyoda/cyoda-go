package parity

// OIDC provider management parity scenarios — Phase 9.2 (#284).
//
// Rows 1-6:  CRUD happy-path (register, list-all, list-active-only,
//             update-issuers, invalidate, delete).
// Rows 7-10: CRUD negative (404/duplicate).
// Rows 11-16: Authz negative — non-admin token → 403 FORBIDDEN.
//
// All scenarios require only a fresh admin tenant (fix.NewTenant) plus
// an OIDC-capable cyoda binary. The three authz-negative helpers
// additionally require fix to implement NonAdminTenantFixture; if not,
// the scenario skips via NonAdminTenantOrSkip.
//
// URI convention: tests use http://fake-oidc-{n}.example.test/... where
// the .example.test TLD never resolves in production DNS. The fixture
// environment sets CYODA_OIDC_REQUIRE_HTTPS=false and
// CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true so validation passes without
// network I/O.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// oidcWellKnownURI returns a stable-per-test fake OIDC discovery URI.
// The salt is combined with the tenant ID to avoid cross-tenant
// collision when tests run against the same backend instance.
func oidcWellKnownURI(tenantID, salt string) string {
	return fmt.Sprintf("http://fake-oidc.example.test/%s/%s/.well-known/openid-configuration", tenantID, salt)
}

// assertErrCode verifies the RFC 9457 Problem Details error envelope in raw
// carries the expected errorCode value in properties.errorCode.
// Only called on non-2xx responses.
func assertErrCode(t *testing.T, raw []byte, wantCode string) {
	t.Helper()
	var envelope struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Errorf("assertErrCode: failed to decode error body %q: %v", string(raw), err)
		return
	}
	if envelope.Properties.ErrorCode != wantCode {
		t.Errorf("properties.errorCode: got %q, want %q (body: %s)", envelope.Properties.ErrorCode, wantCode, string(raw))
	}
}

// --- CRUD happy-path (rows 1-6) ---

// RunOidcRegister verifies POST /oauth/oidc/providers returns 200 with a
// populated OidcProviderResponseDto (id, wellKnownConfigUri, active=true,
// createdAt non-zero).
func RunOidcRegister(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "register")
	p, err := c.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": uri,
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	if p.ID == uuid.Nil {
		t.Error("id must be non-nil UUID")
	}
	if p.WellKnownConfigUri != uri {
		t.Errorf("wellKnownConfigUri: got %q, want %q", p.WellKnownConfigUri, uri)
	}
	if !p.Active {
		t.Error("active must be true on registration")
	}
	if p.CreatedAt.IsZero() {
		t.Error("createdAt must be populated")
	}
}

// RunOidcListAll verifies GET /oauth/oidc/providers returns all registered
// providers for the tenant. Registers 2, expects the list to have at least 2.
func RunOidcListAll(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri1 := oidcWellKnownURI(tenant.ID, "list-all-1")
	uri2 := oidcWellKnownURI(tenant.ID, "list-all-2")

	p1, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri1})
	if err != nil {
		t.Fatalf("RegisterOidcProvider 1: %v", err)
	}
	p2, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri2})
	if err != nil {
		t.Fatalf("RegisterOidcProvider 2: %v", err)
	}

	providers, err := c.ListOidcProviders(t, false)
	if err != nil {
		t.Fatalf("ListOidcProviders: %v", err)
	}

	found1, found2 := false, false
	for _, p := range providers {
		if p.ID == p1.ID {
			found1 = true
		}
		if p.ID == p2.ID {
			found2 = true
		}
	}
	if !found1 {
		t.Errorf("provider 1 (%s) missing from list", p1.ID)
	}
	if !found2 {
		t.Errorf("provider 2 (%s) missing from list", p2.ID)
	}
}

// RunOidcListActiveOnly verifies GET /oauth/oidc/providers?activeOnly=true
// returns only active providers. Registers 2, invalidates 1, expects 1 in list.
func RunOidcListActiveOnly(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uriActive := oidcWellKnownURI(tenant.ID, "list-active-keep")
	uriInactive := oidcWellKnownURI(tenant.ID, "list-active-inactivate")

	pActive, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uriActive})
	if err != nil {
		t.Fatalf("RegisterOidcProvider active: %v", err)
	}
	pInactive, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uriInactive})
	if err != nil {
		t.Fatalf("RegisterOidcProvider inactive: %v", err)
	}

	if err := c.InvalidateOidcProvider(t, pInactive.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	providers, err := c.ListOidcProviders(t, true)
	if err != nil {
		t.Fatalf("ListOidcProviders(activeOnly=true): %v", err)
	}

	for _, p := range providers {
		if p.ID == pInactive.ID {
			t.Errorf("invalidated provider %s must not appear in activeOnly=true list", pInactive.ID)
		}
	}

	found := false
	for _, p := range providers {
		if p.ID == pActive.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("active provider %s missing from activeOnly=true list", pActive.ID)
	}
}

// RunOidcUpdateIssuers verifies PATCH /oauth/oidc/providers/{id} updates
// the issuers field and returns the updated provider.
func RunOidcUpdateIssuers(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "update-issuers")
	p, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	updated, err := c.UpdateOidcProvider(t, p.ID, map[string]any{
		"issuers": []string{"https://issuer.example.test"},
	})
	if err != nil {
		t.Fatalf("UpdateOidcProvider: %v", err)
	}

	if updated.Issuers == nil {
		t.Fatal("updated Issuers must be non-nil")
	}
	issuers := *updated.Issuers
	if len(issuers) != 1 || issuers[0] != "https://issuer.example.test" {
		t.Errorf("issuers: got %v, want [https://issuer.example.test]", issuers)
	}
	if updated.WellKnownConfigUri != uri {
		t.Errorf("wellKnownConfigUri changed unexpectedly: got %q", updated.WellKnownConfigUri)
	}
}

// RunOidcInvalidate verifies POST /oauth/oidc/providers/{id}/invalidate
// returns 200 empty and the subsequent list shows active=false.
func RunOidcInvalidate(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "invalidate")
	p, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}
	if !p.Active {
		t.Fatal("provider must be active before invalidation")
	}

	if err := c.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	// Verify via listing that the provider is now inactive.
	providers, err := c.ListOidcProviders(t, false)
	if err != nil {
		t.Fatalf("ListOidcProviders: %v", err)
	}
	for _, listed := range providers {
		if listed.ID == p.ID {
			if listed.Active {
				t.Errorf("provider %s active flag: got true, want false after invalidation", p.ID)
			}
			return
		}
	}
	t.Errorf("invalidated provider %s not found in list", p.ID)
}

// RunOidcDelete verifies DELETE /oauth/oidc/providers/{id} returns 200 empty
// and a subsequent list no longer contains the provider.
func RunOidcDelete(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "delete")
	p, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	if err := c.DeleteOidcProvider(t, p.ID); err != nil {
		t.Fatalf("DeleteOidcProvider: %v", err)
	}

	providers, err := c.ListOidcProviders(t, false)
	if err != nil {
		t.Fatalf("ListOidcProviders after delete: %v", err)
	}
	for _, listed := range providers {
		if listed.ID == p.ID {
			t.Errorf("deleted provider %s still in list", p.ID)
		}
	}
}

// --- CRUD negative (rows 7-10) ---

// RunOidcUpdateNonExistent verifies PATCH /oauth/oidc/providers/{random-uuid}
// returns 404 with code OIDC_PROVIDER_NOT_FOUND.
func RunOidcUpdateNonExistent(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	bogusID := uuid.New()
	status, raw, err := c.UpdateOidcProviderRaw(t, bogusID, map[string]any{
		"issuers": []string{"https://issuer.example.test"},
	})
	if err != nil {
		t.Fatalf("UpdateOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "OIDC_PROVIDER_NOT_FOUND")
}

// RunOidcInvalidateNonExistent verifies
// POST /oauth/oidc/providers/{random-uuid}/invalidate returns 404 with code
// OIDC_PROVIDER_NOT_FOUND.
func RunOidcInvalidateNonExistent(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	bogusID := uuid.New()
	status, raw, err := c.InvalidateOidcProviderRaw(t, bogusID)
	if err != nil {
		t.Fatalf("InvalidateOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "OIDC_PROVIDER_NOT_FOUND")
}

// RunOidcReactivateNonExistent verifies
// POST /oauth/oidc/providers/{random-uuid}/reactivate returns 404 with code
// OIDC_PROVIDER_NOT_FOUND.
func RunOidcReactivateNonExistent(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	bogusID := uuid.New()
	status, raw, err := c.ReactivateOidcProviderRaw(t, bogusID)
	if err != nil {
		t.Fatalf("ReactivateOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "OIDC_PROVIDER_NOT_FOUND")
}

// RunOidcDuplicateRegister verifies that registering a second provider with
// the same wellKnownConfigUri under the same tenant returns 400 with code
// OIDC_PROVIDER_DUPLICATE.
func RunOidcDuplicateRegister(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "duplicate")
	if _, err := c.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri}); err != nil {
		t.Fatalf("RegisterOidcProvider first: %v", err)
	}

	status, raw, err := c.RegisterOidcProviderRaw(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("RegisterOidcProviderRaw second transport: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "OIDC_PROVIDER_DUPLICATE")
}

// --- Authz negative (rows 11-16) ---

// RunOidcNonAdminRegister verifies that a non-admin token is rejected
// (403 FORBIDDEN) on POST /oauth/oidc/providers.
func RunOidcNonAdminRegister(t *testing.T, fix BackendFixture) {
	nonAdmin := NonAdminTenantOrSkip(t, fix)
	c := client.NewClient(fix.BaseURL(), nonAdmin.Token)

	uri := oidcWellKnownURI(nonAdmin.ID, "nonadmin-register")
	status, raw, err := c.RegisterOidcProviderRaw(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("RegisterOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "FORBIDDEN")
}

// RunOidcNonAdminUpdate verifies that a non-admin token is rejected
// (403 FORBIDDEN) on PATCH /oauth/oidc/providers/{id}.
func RunOidcNonAdminUpdate(t *testing.T, fix BackendFixture) {
	nonAdmin := NonAdminTenantOrSkip(t, fix)

	// Register a provider as admin first so there's a real ID to target.
	admin := fix.NewTenant(t)
	adminClient := client.NewClient(fix.BaseURL(), admin.Token)
	uri := oidcWellKnownURI(admin.ID, "nonadmin-update")
	p, err := adminClient.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("admin RegisterOidcProvider: %v", err)
	}

	// Attempt update with non-admin client.
	c := client.NewClient(fix.BaseURL(), nonAdmin.Token)
	status, raw, err := c.UpdateOidcProviderRaw(t, p.ID, map[string]any{
		"issuers": []string{"https://issuer.example.test"},
	})
	if err != nil {
		t.Fatalf("UpdateOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "FORBIDDEN")
}

// RunOidcNonAdminInvalidate verifies that a non-admin token is rejected
// (403 FORBIDDEN) on POST /oauth/oidc/providers/{id}/invalidate.
func RunOidcNonAdminInvalidate(t *testing.T, fix BackendFixture) {
	nonAdmin := NonAdminTenantOrSkip(t, fix)

	admin := fix.NewTenant(t)
	adminClient := client.NewClient(fix.BaseURL(), admin.Token)
	uri := oidcWellKnownURI(admin.ID, "nonadmin-invalidate")
	p, err := adminClient.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("admin RegisterOidcProvider: %v", err)
	}

	c := client.NewClient(fix.BaseURL(), nonAdmin.Token)
	status, raw, err := c.InvalidateOidcProviderRaw(t, p.ID)
	if err != nil {
		t.Fatalf("InvalidateOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "FORBIDDEN")
}

// RunOidcNonAdminReactivate verifies that a non-admin token is rejected
// (403 FORBIDDEN) on POST /oauth/oidc/providers/{id}/reactivate.
func RunOidcNonAdminReactivate(t *testing.T, fix BackendFixture) {
	nonAdmin := NonAdminTenantOrSkip(t, fix)

	admin := fix.NewTenant(t)
	adminClient := client.NewClient(fix.BaseURL(), admin.Token)
	uri := oidcWellKnownURI(admin.ID, "nonadmin-reactivate")
	p, err := adminClient.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("admin RegisterOidcProvider: %v", err)
	}
	// Invalidate so reactivate is meaningful (even though the non-admin
	// call should be rejected before the state is checked).
	if err := adminClient.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("admin InvalidateOidcProvider: %v", err)
	}

	c := client.NewClient(fix.BaseURL(), nonAdmin.Token)
	status, raw, err := c.ReactivateOidcProviderRaw(t, p.ID)
	if err != nil {
		t.Fatalf("ReactivateOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "FORBIDDEN")
}

// RunOidcNonAdminDelete verifies that a non-admin token is rejected
// (403 FORBIDDEN) on DELETE /oauth/oidc/providers/{id}.
func RunOidcNonAdminDelete(t *testing.T, fix BackendFixture) {
	nonAdmin := NonAdminTenantOrSkip(t, fix)

	admin := fix.NewTenant(t)
	adminClient := client.NewClient(fix.BaseURL(), admin.Token)
	uri := oidcWellKnownURI(admin.ID, "nonadmin-delete")
	p, err := adminClient.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": uri})
	if err != nil {
		t.Fatalf("admin RegisterOidcProvider: %v", err)
	}

	c := client.NewClient(fix.BaseURL(), nonAdmin.Token)
	status, raw, err := c.DeleteOidcProviderRaw(t, p.ID)
	if err != nil {
		t.Fatalf("DeleteOidcProviderRaw transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "FORBIDDEN")
}

// RunOidcNonAdminReload verifies that a non-admin token is rejected
// (403 FORBIDDEN) on POST /oauth/oidc/providers/reload.
func RunOidcNonAdminReload(t *testing.T, fix BackendFixture) {
	nonAdmin := NonAdminTenantOrSkip(t, fix)
	c := client.NewClient(fix.BaseURL(), nonAdmin.Token)

	status, raw, err := c.ReloadOidcProvidersRaw(t)
	if err != nil {
		t.Fatalf("ReloadOidcProvidersRaw transport: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %s)", status, raw)
	}
	assertErrCode(t, raw, "FORBIDDEN")
}

// --- JWT validation integration (rows 17-20) ---
//
// These scenarios spin up an in-process mock IdP (ParityFixtureIdP on a
// random localhost port), register it as an OIDC provider, and then verify
// that JWTs signed by the mock are accepted or rejected as expected.
//
// The parity test subprocess runs with CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true
// and CYODA_OIDC_REQUIRE_HTTPS=false so the 127.0.0.1 discovery URL passes
// the SSRF check without external network I/O.
//
// For token validation probing we use GET /api/oauth/oidc/providers, which
// is the lightest authenticated endpoint — it requires a valid JWT but no
// specific role, and returns 200 on success / 401 on auth failure.
//
// Tenant isolation note: when an OIDC-signed JWT is accepted, the
// UserContext.Tenant.ID is set to the provider's OwnerLegalEntityID (the
// admin tenant that registered the provider), NOT to any claim in the JWT.
// The probe therefore lists providers for the admin tenant, which is correct.

// assertProbeStatus asserts that the given probe response matches the expected
// HTTP status code. It is used by JWT validation parity tests.
func assertProbeStatus(t *testing.T, wantStatus, gotStatus int, body []byte) {
	t.Helper()
	if gotStatus != wantStatus {
		t.Errorf("probe status: got %d, want %d (body: %s)", gotStatus, wantStatus, string(body))
	}
}

// RunOidcJWTValidation_RegisterAndAccept verifies the end-to-end happy path:
// register a mock IdP provider, sign a JWT with the mock, assert GET
// /api/oauth/oidc/providers returns 200 with that JWT (row 17).
func RunOidcJWTValidation_RegisterAndAccept(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("ProbeAuthRaw transport: %v", err)
	}
	_ = p
	assertProbeStatus(t, http.StatusOK, status, body)
}

// RunOidcJWTValidation_InvalidateRejects verifies that invalidating a provider
// causes subsequent JWT validation to fail with 401 (row 18).
func RunOidcJWTValidation_InvalidateRejects(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Baseline: accepted.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("baseline ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Invalidate the provider.
	if err := adminC.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	// Now the same token must be rejected.
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("post-invalidate ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// RunOidcJWTValidation_ReactivateRecovers verifies that reactivating a
// previously invalidated provider restores JWT acceptance (row 19).
func RunOidcJWTValidation_ReactivateRecovers(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Step 1: token accepted.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step1 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Step 2: invalidate → rejected.
	if err := adminC.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step2 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)

	// Step 3: reactivate → accepted again.
	if _, err := adminC.ReactivateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("ReactivateOidcProvider: %v", err)
	}
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step3 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)
}

// RunOidcJWTValidation_DeletePermanent verifies that deleting a provider
// permanently rejects its JWTs even after the same URI is not re-registered
// (row 20).
func RunOidcJWTValidation_DeletePermanent(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Baseline: accepted.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("baseline ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Delete the provider permanently.
	if err := adminC.DeleteOidcProvider(t, p.ID); err != nil {
		t.Fatalf("DeleteOidcProvider: %v", err)
	}

	// JWT must now be permanently rejected.
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("post-delete ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// --- Issuer-list update affects validation (row 21) ---

// RunOidcJWTValidation_IssuerListUpdate verifies that updating a provider's
// issuers pin list rejects previously-valid JWTs whose iss no longer matches
// (row 21).
//
// Sequence:
//  1. Register with no issuers pin (accepts any iss == discovery doc's issuer).
//  2. Sign JWT with iss = mock IdP's issuer → accepted.
//  3. Update issuers to a different value that does NOT match the mock's issuer.
//  4. Same JWT → rejected (401) because iss no longer matches the pin list.
func RunOidcJWTValidation_IssuerListUpdate(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Step 1: no issuer pin → accepted.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step1 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Step 2: pin issuers to a value that does NOT match the mock's issuer.
	// The mock's issuer is idp.Issuer (e.g. "http://127.0.0.1:NNNNN").
	// We set issuers to a different value so the existing token's iss claim
	// no longer matches.
	if _, err := adminC.UpdateOidcProvider(t, p.ID, map[string]any{
		"issuers": []string{"https://other-issuer.example.test"},
	}); err != nil {
		t.Fatalf("UpdateOidcProvider (set issuers): %v", err)
	}

	// After the issuers pin changes, the registry must reload the provider.
	// Invalidate + reactivate forces the discovery/JWKS path to re-run so
	// the updated provider config is reflected in the in-memory cache.
	if err := adminC.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}
	if _, err := adminC.ReactivateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("ReactivateOidcProvider: %v", err)
	}

	// Step 3: same token → rejected because iss no longer in pin list.
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step3 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// --- Key rotation/revocation (rows 22-26b) ---

// RunOidcKeyRotation_NewKidAccepted verifies that after a key rotation at the
// mock IdP (new kid added to JWKS), a JWT signed with the new kid is accepted
// (row 22). The registry fetches the updated JWKS on the cold path when it
// encounters an unseen kid.
func RunOidcKeyRotation_NewKidAccepted(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	if _, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	}); err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	// Rotate: add a new key (does NOT remove the old one).
	newKid := idp.RotateKey(t)

	// Sign JWT with the new kid — it is NOT yet cached by the registry.
	token := idp.MintTenantJWT(t, newKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Cold path: registry iterates all providers, calls GetKey(newKid),
	// HTTPJWKSSource cache-miss → refreshCache() → new kid found → 200.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)
}

// RunOidcKeyRotation_OldKidStillAccepted verifies that the old kid continues
// to work after a key rotation (new kid added, old key not revoked) (row 23).
func RunOidcKeyRotation_OldKidStillAccepted(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	if _, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	}); err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	// Rotate: new kid added, old DefaultKid remains in JWKS.
	_ = idp.RotateKey(t)

	// Sign JWT with the original DefaultKid.
	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// DefaultKid is still in JWKS → 200.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)
}

// RunOidcKeyRevocation_RevokedKidRejected verifies that after a kid is revoked
// at the mock IdP and the provider cache is cleared (via invalidate+reactivate),
// JWTs signed with the revoked kid are rejected with 401 (row 24).
//
// Revocation is modelled as removing the kid from the JWKS endpoint. The
// local cache is cleared by invalidating and reactivating the provider, which
// causes the registry to create a fresh HTTPJWKSSource with an empty cache.
// The first GetKey call after reactivation fetches the updated JWKS, which no
// longer includes the revoked kid → ErrKeyNotFound → 401.
func RunOidcKeyRevocation_RevokedKidRejected(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Baseline: accepted (warms the JWKS cache with DefaultKid).
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("baseline ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Revoke DefaultKid at the mock IdP (removes it from future JWKS responses).
	idp.RevokeKey(t, idp.DefaultKid)

	// Invalidate the provider (drops the cached source from the registry).
	if err := adminC.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	// Reactivate (creates a fresh HTTPJWKSSource with empty cache).
	// Use reactivateKeys=false to avoid a JWKS sync that could populate the cache
	// with the (now-revoked) kid from a stale server response. The cache is
	// already empty because the source was just created.
	if _, err := adminC.ReactivateOidcProviderWithKeys(t, p.ID, false); err != nil {
		t.Fatalf("ReactivateOidcProviderWithKeys: %v", err)
	}

	// Now the JWKS cache is empty. The next GetKey call fetches the updated
	// JWKS, which does NOT contain DefaultKid → 401.
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("post-revocation ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// RunOidcKeyRotation_ColdStartReturnsErrUnknownKID verifies that a JWT
// whose kid is not present in any registered provider's JWKS is rejected
// with 401 (row 25).
//
// The scenario uses a JWT whose kid header names a key that the mock IdP
// has never published (the JWT is structurally valid but signed by a key
// the registry has no way to verify). The cold path iterates all providers,
// calls GetKey for the unknown kid, finds nothing → ErrUnknownKID → chain
// falls through → 401.
func RunOidcKeyRotation_ColdStartReturnsErrUnknownKID(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	// The "known" IdP is registered — its JWKS has knownIdP.DefaultKid.
	knownIdP := NewParityFixtureIdP(t)
	if _, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": knownIdP.WellKnownURI(),
	}); err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	// Create a second (unregistered) IdP that signs with its own key. We sign a
	// JWT whose iss matches the registered provider but whose kid is from the
	// unregistered IdP's key pair — the registry will not find this kid in any
	// JWKS endpoint it knows about.
	unknownIdP := NewParityFixtureIdP(t)

	// Mint a JWT with the unknown IdP's signing key but claim the known IdP's
	// issuer so the registry attempts resolution via the known provider's JWKS.
	// RotateKey generates a fresh kid guaranteed to be absent from knownIdP's JWKS
	// → cold path finds nothing → ErrUnknownKID → 401.
	foreignKid := unknownIdP.RotateKey(t)
	token := unknownIdP.MintTenantJWTWithIssuer(t, foreignKid, admin.ID, knownIdP.Issuer)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Cold path: kid not in any JWKS → 401.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// RunOidcReactivate_RemoteRemovalSync verifies D19's conditional JWKS sync:
// when reactivating with reactivateKeys=true and the upstream has removed a
// kid, the local cache drops that kid (row 26a).
//
// Sequence:
//  1. Register mock IdP; warm the JWKS cache (first probe succeeds).
//  2. Invalidate the provider (drops source from registry).
//  3. Revoke DefaultKid at the mock IdP.
//  4. Reactivate with reactivateKeys=true.
//  5. JWT with DefaultKid is now rejected.
func RunOidcReactivate_RemoteRemovalSync(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Warm JWKS cache — accepted.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step1 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Invalidate the provider.
	if err := adminC.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	// Revoke DefaultKid at the mock IdP so future JWKS responses omit it.
	idp.RevokeKey(t, idp.DefaultKid)

	// Reactivate with reactivateKeys=true: D19 sync fetches updated JWKS and
	// drops the revoked kid from the local cache.
	if _, err := adminC.ReactivateOidcProviderWithKeys(t, p.ID, true); err != nil {
		t.Fatalf("ReactivateOidcProviderWithKeys(true): %v", err)
	}

	// JWT with DefaultKid rejected (kid not in updated JWKS).
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step5 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// RunOidcReactivate_RemoteKeysPreservedSync verifies D19's idempotency: when
// reactivating with reactivateKeys=true and the upstream JWKS is unchanged,
// previously-valid JWTs continue to be accepted (row 26b).
//
// Sequence:
//  1. Register mock IdP; warm the JWKS cache (first probe succeeds).
//  2. Invalidate the provider.
//  3. Reactivate with reactivateKeys=true — JWKS unchanged, so DefaultKid
//     is still present after the sync.
//  4. JWT with DefaultKid still accepted.
func RunOidcReactivate_RemoteKeysPreservedSync(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	token := idp.MintTenantJWT(t, idp.DefaultKid, admin.ID)
	probeC := client.NewClient(fix.BaseURL(), token)

	// Warm JWKS cache.
	status, body, err := probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step1 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Invalidate.
	if err := adminC.InvalidateOidcProvider(t, p.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	// Reactivate with reactivateKeys=true — JWKS unchanged, DefaultKid still there.
	if _, err := adminC.ReactivateOidcProviderWithKeys(t, p.ID, true); err != nil {
		t.Fatalf("ReactivateOidcProviderWithKeys(true): %v", err)
	}

	// JWT with DefaultKid still accepted (JWKS unchanged).
	status, body, err = probeC.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("step4 ProbeAuthRaw transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)
}

// --- Multi-provider isolation (row 27) ---

// RunOidcMultiProvider_Isolation verifies that two OIDC providers for the
// same tenant validate only their own JWTs — cross-signing (JWT signed by
// provider A's key but claiming provider B's issuer) must be rejected (row 27).
//
// Sequence:
//  1. Register two mock IdPs (A and B) for the same tenant.
//  2. JWT signed by A's key, iss=A.Issuer → accepted.
//  3. JWT signed by B's key, iss=B.Issuer → accepted.
//  4. JWT signed by A's key, iss=B.Issuer (cross-sign) → rejected (401)
//     because A's public key does not verify a signature made by B.
//
// This test pins the chain's iss-based routing: each provider is only
// eligible for tokens whose iss matches its discovery-doc issuer (or pin
// list). Provider A's source has A's public key; provider B's source has
// B's public key. A JWT claiming B's iss is routed to B's source, which
// then fails to verify A's signature.
func RunOidcMultiProvider_Isolation(t *testing.T, fix BackendFixture) {
	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idpA := NewParityFixtureIdP(t)
	idpB := NewParityFixtureIdP(t)

	if _, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idpA.WellKnownURI(),
	}); err != nil {
		t.Fatalf("RegisterOidcProvider A: %v", err)
	}
	if _, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idpB.WellKnownURI(),
	}); err != nil {
		t.Fatalf("RegisterOidcProvider B: %v", err)
	}

	// Step 1: JWT from A → accepted.
	tokenA := idpA.MintTenantJWT(t, idpA.DefaultKid, admin.ID)
	probeA := client.NewClient(fix.BaseURL(), tokenA)
	status, body, err := probeA.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("probeA transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Step 2: JWT from B → accepted.
	tokenB := idpB.MintTenantJWT(t, idpB.DefaultKid, admin.ID)
	probeB := client.NewClient(fix.BaseURL(), tokenB)
	status, body, err = probeB.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("probeB transport: %v", err)
	}
	assertProbeStatus(t, http.StatusOK, status, body)

	// Step 3: cross-sign — signed by A's key, claims B's issuer → rejected.
	// The registry routes the token to B's provider (because iss=B.Issuer),
	// then tries B's public key against A's signature → verification fails.
	crossToken := idpA.MintTenantJWTWithIssuer(t, idpA.DefaultKid, admin.ID, idpB.Issuer)
	probeCross := client.NewClient(fix.BaseURL(), crossToken)
	status, body, err = probeCross.ProbeAuthRaw(t)
	if err != nil {
		t.Fatalf("probeCross transport: %v", err)
	}
	assertProbeStatus(t, http.StatusUnauthorized, status, body)
}

// assertErrCodeOptional logs an error code mismatch as a note without
// failing the test — used in JWT-validation scenarios where the error body
// shape is not the primary assertion (status code is sufficient).
func assertErrCodeOptional(t *testing.T, raw []byte, wantCode string) {
	t.Helper()
	var envelope struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// 401 responses from the auth middleware may not be Problem Detail format.
		return
	}
	if envelope.Properties.ErrorCode != "" && envelope.Properties.ErrorCode != wantCode {
		t.Logf("note: properties.errorCode got %q, expected %q (non-fatal)", envelope.Properties.ErrorCode, wantCode)
	}
}
