package e2e_test

// oidc_providers_test.go — exercise-level e2e coverage for the 7 OIDC
// provider operations (operationIds: registerOidcProvider, listOidcProviders,
// reloadOidcProviders, updateOidcProvider, invalidateOidcProvider,
// reactivateOidcProvider, deleteOidcProvider).
//
// Coverage level: each op hit ≥1 time, 2xx asserted. Exhaustive error-code
// coverage is deferred to the auth/OIDC follow-on group.
//
// Design notes:
//
//   - CYODA_OIDC_REQUIRE_HTTPS and CYODA_OIDC_ALLOW_PRIVATE_NETWORKS are set
//     via init() (runs before TestMain) so the app starts with HTTP and
//     private-network OIDC URIs allowed, making it possible to register fake
//     providers without a real HTTPS endpoint.  No existing e2e test exercises
//     OIDC SSRF/TLS enforcement (those are unit tests in internal/auth/oidc).
//
//   - registerOidcProvider requires a UUID-shaped tenant identifier.  The
//     bootstrap tenant ("test-tenant") is not UUID-shaped, so this test seeds
//     a dedicated M2M client with a real UUID tenant via createM2MClient and
//     drives every OIDC call with that client's token.
//
//   - When registration succeeds the OIDC registry attempts to fetch the
//     discovery document (reloadOne); failure there is WARN-logged and
//     non-fatal — the provider is stored and all subsequent lifecycle ops
//     work against the KV store, not the discovery endpoint.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func init() {
	// Allow HTTP (non-HTTPS) and private-network OIDC URIs in the app under
	// test.  Both flags are read by app.DefaultConfig() which is called from
	// TestMain — this init() runs first, before TestMain.
	os.Setenv("CYODA_OIDC_REQUIRE_HTTPS", "false")
	os.Setenv("CYODA_OIDC_ALLOW_PRIVATE_NETWORKS", "true")
}

// oidcTenantUUID is the UUID-shaped tenant used exclusively by the OIDC
// lifecycle tests.  A hardcoded value keeps the test deterministic and avoids
// polluting the list with a random UUID on every run.
const oidcTenantUUID = "e2e00000-0000-0000-0000-000000000001"

// TestOidcProviderLifecycle exercises all 7 OIDC provider operations in a
// single lifecycle flow:
//
//	registerOidcProvider → listOidcProviders → reloadOidcProviders →
//	updateOidcProvider → invalidateOidcProvider → reactivateOidcProvider →
//	deleteOidcProvider
//
// Each step asserts a 2xx response.
func TestOidcProviderLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	// Seed a client with a UUID-shaped tenant so registerOidcProvider does
	// not fail with OIDC_INVALID_TENANT.  The roles include ROLE_ADMIN so
	// all admin-gated OIDC ops are permitted.
	clientID, clientSecret := createM2MClient(t, oidcTenantUUID, "oidc-e2e-user", []string{"ROLE_ADMIN", "ROLE_M2M"})

	// oidcDo issues an authenticated HTTP request using the UUID-tenant client.
	// path must NOT include the /api prefix (adminRequestAs adds it).
	oidcDo := func(t *testing.T, method, path string, body []byte) *http.Response {
		t.Helper()
		resp := adminRequestAs(t, clientID, clientSecret, method, path, body)
		return resp
	}

	// Unique fake URI so repeated runs don't collide on the duplicate check.
	wellKnown := fmt.Sprintf("http://oidc-e2e-lifecycle-%d.local/.well-known/openid-configuration",
		time.Now().UnixNano())

	// ── Step 1: registerOidcProvider ─────────────────────────────────────────
	registerBody := mustJSON(t, map[string]any{
		"wellKnownConfigUri": wellKnown,
	})
	registerResp := oidcDo(t, http.MethodPost, "/oauth/oidc/providers", registerBody)
	registerRaw, _ := io.ReadAll(registerResp.Body)
	registerResp.Body.Close()
	if registerResp.StatusCode != http.StatusOK {
		t.Fatalf("registerOidcProvider: expected 200, got %d: %s", registerResp.StatusCode, registerRaw)
	}

	var registered struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(registerRaw, &registered); err != nil {
		t.Fatalf("registerOidcProvider: decode response: %v; body: %s", err, registerRaw)
	}
	if registered.ID == "" {
		t.Fatal("registerOidcProvider: expected non-empty id in response")
	}
	providerID := registered.ID

	// ── Step 2: listOidcProviders ─────────────────────────────────────────────
	listResp := oidcDo(t, http.MethodGet, "/oauth/oidc/providers", nil)
	listRaw, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("listOidcProviders: expected 200, got %d: %s", listResp.StatusCode, listRaw)
	}
	// Response must be a JSON array.
	var listed []map[string]any
	if err := json.Unmarshal(listRaw, &listed); err != nil {
		t.Fatalf("listOidcProviders: expected JSON array; parse error: %v; body: %s", err, listRaw)
	}

	// ── Step 3: reloadOidcProviders ───────────────────────────────────────────
	reloadResp := oidcDo(t, http.MethodPost, "/oauth/oidc/providers/reload", nil)
	reloadRaw, _ := io.ReadAll(reloadResp.Body)
	reloadResp.Body.Close()
	if reloadResp.StatusCode != http.StatusOK {
		t.Fatalf("reloadOidcProviders: expected 200, got %d: %s", reloadResp.StatusCode, reloadRaw)
	}

	// ── Step 4: updateOidcProvider ────────────────────────────────────────────
	updateBody := mustJSON(t, map[string]any{
		"issuers": []string{"https://issuer.oidc-e2e.local"},
	})
	updatePath := fmt.Sprintf("/oauth/oidc/providers/%s", providerID)
	updateResp := oidcDo(t, http.MethodPatch, updatePath, updateBody)
	updateRaw, _ := io.ReadAll(updateResp.Body)
	updateResp.Body.Close()
	if updateResp.StatusCode != http.StatusOK {
		t.Fatalf("updateOidcProvider: expected 200, got %d: %s", updateResp.StatusCode, updateRaw)
	}

	// ── Step 5: invalidateOidcProvider ────────────────────────────────────────
	invalidatePath := fmt.Sprintf("/oauth/oidc/providers/%s/invalidate", providerID)
	invalidateResp := oidcDo(t, http.MethodPost, invalidatePath, nil)
	invalidateRaw, _ := io.ReadAll(invalidateResp.Body)
	invalidateResp.Body.Close()
	if invalidateResp.StatusCode != http.StatusOK {
		t.Fatalf("invalidateOidcProvider: expected 200, got %d: %s", invalidateResp.StatusCode, invalidateRaw)
	}

	// ── Step 6: reactivateOidcProvider ───────────────────────────────────────
	reactivatePath := fmt.Sprintf("/oauth/oidc/providers/%s/reactivate", providerID)
	reactivateResp := oidcDo(t, http.MethodPost, reactivatePath, nil)
	reactivateRaw, _ := io.ReadAll(reactivateResp.Body)
	reactivateResp.Body.Close()
	if reactivateResp.StatusCode != http.StatusOK {
		t.Fatalf("reactivateOidcProvider: expected 200, got %d: %s", reactivateResp.StatusCode, reactivateRaw)
	}

	// ── Step 7: deleteOidcProvider ────────────────────────────────────────────
	deletePath := fmt.Sprintf("/oauth/oidc/providers/%s", providerID)
	deleteResp := oidcDo(t, http.MethodDelete, deletePath, nil)
	deleteRaw, _ := io.ReadAll(deleteResp.Body)
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusOK {
		t.Fatalf("deleteOidcProvider: expected 200, got %d: %s", deleteResp.StatusCode, deleteRaw)
	}
}

// oidcUppercaseTenantUUID is the same class of identifier as oidcTenantUUID —
// a real UUID — spelled in upper case.  The tenant grammar admits it, so a
// deployment whose caas_org_id is written this way is a deployment that
// exists, not a hypothetical.
const oidcUppercaseTenantUUID = "E2E00000-0000-0000-0000-000000000002"

// TestOidcProviderLifecycle_UppercaseUUIDTenant drives the full provider
// lifecycle as a tenant whose UUID is spelled in upper case.
//
// The store keys a provider by OwnerLegalEntityID.String(), which is always
// canonical lowercase, and used to read it back under the caller's raw tenant
// string.  For this tenant the two differed: registration wrote one key and
// every later operation addressed another, so the provider was unreachable the
// moment it was created.  Each step below asserts a 2xx, and the list steps
// assert the provider is actually there and actually gone.
func TestOidcProviderLifecycle_UppercaseUUIDTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	clientID, clientSecret := createM2MClient(t, oidcUppercaseTenantUUID, "oidc-upper-user",
		[]string{"ROLE_ADMIN", "ROLE_M2M"})

	do := func(t *testing.T, method, path string, body []byte) (int, []byte) {
		t.Helper()
		resp := adminRequestAs(t, clientID, clientSecret, method, path, body)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}

	// listIDs returns the provider ids this tenant can see.
	listIDs := func(t *testing.T) []string {
		t.Helper()
		status, raw := do(t, http.MethodGet, "/oauth/oidc/providers", nil)
		if status != http.StatusOK {
			t.Fatalf("listOidcProviders: expected 200, got %d: %s", status, raw)
		}
		var listed []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &listed); err != nil {
			t.Fatalf("listOidcProviders: expected JSON array; parse error: %v; body: %s", err, raw)
		}
		ids := make([]string, 0, len(listed))
		for _, p := range listed {
			ids = append(ids, p.ID)
		}
		return ids
	}

	wellKnown := fmt.Sprintf("http://oidc-e2e-upper-%d.local/.well-known/openid-configuration",
		time.Now().UnixNano())

	status, raw := do(t, http.MethodPost, "/oauth/oidc/providers",
		mustJSON(t, map[string]any{"wellKnownConfigUri": wellKnown}))
	if status != http.StatusOK {
		t.Fatalf("registerOidcProvider: expected 200, got %d: %s", status, raw)
	}
	var registered struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &registered); err != nil {
		t.Fatalf("registerOidcProvider: decode response: %v; body: %s", err, raw)
	}
	if registered.ID == "" {
		t.Fatal("registerOidcProvider: expected non-empty id in response")
	}
	providerID := registered.ID

	if !containsString(listIDs(t), providerID) {
		t.Fatalf("listOidcProviders: provider %s is missing — it was registered under a key this tenant cannot address", providerID)
	}

	if status, raw := do(t, http.MethodPatch, "/oauth/oidc/providers/"+providerID,
		mustJSON(t, map[string]any{"issuers": []string{"https://issuer.oidc-e2e-upper.local"}})); status != http.StatusOK {
		t.Fatalf("updateOidcProvider: expected 200, got %d: %s", status, raw)
	}
	if status, raw := do(t, http.MethodPost, "/oauth/oidc/providers/"+providerID+"/invalidate", nil); status != http.StatusOK {
		t.Fatalf("invalidateOidcProvider: expected 200, got %d: %s", status, raw)
	}
	if status, raw := do(t, http.MethodPost, "/oauth/oidc/providers/"+providerID+"/reactivate", nil); status != http.StatusOK {
		t.Fatalf("reactivateOidcProvider: expected 200, got %d: %s", status, raw)
	}
	if status, raw := do(t, http.MethodDelete, "/oauth/oidc/providers/"+providerID, nil); status != http.StatusOK {
		t.Fatalf("deleteOidcProvider: expected 200, got %d: %s", status, raw)
	}

	if containsString(listIDs(t), providerID) {
		t.Fatalf("listOidcProviders: provider %s survived its own delete", providerID)
	}
}

// TestOidc_NonUUIDTenant_RejectedOnEveryOperation pins the behaviour change
// that ships with the keying fix.
//
// A non-UUID tenant — here the bootstrap "test-tenant" behind
// testclient/testsecret — used to get an empty 200 from the list endpoint,
// because its prefix scan matched nothing, and a 404 from the id-addressed
// ops.  Both implied a registration that could never have succeeded:
// registration has always answered such a tenant with 400 OIDC_INVALID_TENANT.
// Every provider operation now gives it that same answer.
//
// reloadOidcProviders takes no tenant and is deliberately absent from this
// table; registerOidcProvider is covered by
// TestOIDC_Register_InvalidTenant_ProblemDetail.
func TestOidc_NonUUIDTenant_RejectedOnEveryOperation(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const someProvider = "/oauth/oidc/providers/00000000-0000-0000-0000-0000000000fe"

	for _, op := range []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"listOidcProviders", http.MethodGet, "/oauth/oidc/providers", nil},
		{"updateOidcProvider", http.MethodPatch, someProvider, []byte(`{}`)},
		{"invalidateOidcProvider", http.MethodPost, someProvider + "/invalidate", nil},
		{"reactivateOidcProvider", http.MethodPost, someProvider + "/reactivate", nil},
		{"deleteOidcProvider", http.MethodDelete, someProvider, nil},
	} {
		t.Run(op.name, func(t *testing.T) {
			// testclient/testsecret belong to the bootstrap tenant "test-tenant",
			// which is not UUID-shaped.
			resp := adminRequestAs(t, "testclient", "testsecret", op.method, op.path, op.body)
			assertProblemJSON(t, resp, http.StatusBadRequest, "OIDC_INVALID_TENANT")
		})
	}
}
