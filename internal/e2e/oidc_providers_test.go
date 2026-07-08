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
