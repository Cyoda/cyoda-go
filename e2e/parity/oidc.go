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
