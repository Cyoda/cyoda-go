package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// ─────────────────────────────────────────────────────────────────────────────
// issueJwtKeyPair error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestKeys_IssueNonRS256_400UnsupportedAlgorithm verifies that requesting a
// non-RS256 algorithm returns 400 UNSUPPORTED_ALGORITHM.
func TestKeys_IssueNonRS256_400UnsupportedAlgorithm(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := adminRequest(t, "POST", "/oauth/keys/keypair", mustJSON(t, map[string]any{
		"algorithm": "ES256",
		"audience":  "client",
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "UNSUPPORTED_ALGORITHM")
}

// ─────────────────────────────────────────────────────────────────────────────
// invalidateJwtKeyPair error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestKeys_InvalidateKeyPair_BadGrace_400 verifies that gracePeriodSec < 0
// returns 400 BAD_REQUEST. The grace check runs before the key-store lookup,
// so any keyId (including non-existent) triggers it.
func TestKeys_InvalidateKeyPair_BadGrace_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := adminRequest(t, "POST", "/oauth/keys/keypair/no-such-kid/invalidate", mustJSON(t, map[string]any{
		"gracePeriodSec": int64(-1),
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestKeys_InvalidateKeyPair_UnknownId_404 verifies that invalidating a
// non-existent key returns 404 KEYPAIR_NOT_FOUND.
func TestKeys_InvalidateKeyPair_UnknownId_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := adminRequest(t, "POST", "/oauth/keys/keypair/no-such-kid/invalidate", nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "KEYPAIR_NOT_FOUND")
}

// ─────────────────────────────────────────────────────────────────────────────
// reactivateJwtKeyPair error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestKeys_ReactivateKeyPair_BadBody_400 verifies that omitting the required
// validTo field returns 400 BAD_REQUEST. The validTo check runs before the
// key-store lookup, so any keyId triggers it.
func TestKeys_ReactivateKeyPair_BadBody_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	// {} decodes to zero ValidTo; handler returns 400 "validTo required".
	resp := adminRequest(t, "POST", "/oauth/keys/keypair/no-such-kid/reactivate", mustJSON(t, map[string]any{}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestKeys_ReactivateKeyPair_UnknownId_404 verifies that reactivating a
// non-existent key returns 404 KEYPAIR_NOT_FOUND.
func TestKeys_ReactivateKeyPair_UnknownId_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	body := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/keypair/no-such-kid/reactivate", body)
	assertProblemJSON(t, resp, http.StatusNotFound, "KEYPAIR_NOT_FOUND")
}

// ─────────────────────────────────────────────────────────────────────────────
// registerTrustedKey error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestTrusted_RegisterNonRSA_400UnsupportedKeyType verifies that submitting a
// non-RSA JWK (EC kty) returns 400 UNSUPPORTED_KEY_TYPE.
func TestTrusted_RegisterNonRSA_400UnsupportedKeyType(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	// Minimal EC JWK — server only accepts RSA in this version.
	resp := adminRequest(t, "POST", "/oauth/keys/trusted", mustJSON(t, map[string]any{
		"keyId":    "e2e-ec-type-test",
		"jwk":      map[string]any{"kty": "EC"},
		"audience": "human",
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "UNSUPPORTED_KEY_TYPE")
}

// TestTrustedKey_CapReached_400: registering one key past the per-tenant cap
// (CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT) is 400 TRUSTED_KEY_CAP_REACHED. It runs in a tenant of its
// own, so filling that tenant's cap does not affect any other test.
func TestTrustedKey_CapReached_400(t *testing.T) {
	tenant := fmt.Sprintf("e2e-cap-%d", time.Now().UnixNano())
	clientID, secret := createM2MClient(t, tenant, "cap-admin", []string{"ROLE_ADMIN", "ROLE_M2M"})
	limit := app.DefaultConfig().IAM.TrustedKeyMaxPerTenant

	register := func(i int) *http.Response {
		kid := fmt.Sprintf("%s-%d", tenant, i)
		return adminRequestAs(t, clientID, secret, "POST", "/oauth/keys/trusted",
			mustJSON(t, map[string]any{"keyId": kid, "jwk": rsaJWK(t, kid), "audience": "human"}))
	}
	for i := range limit {
		resp := register(i)
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("key %d of %d: status=%d, want 200; body: %s", i+1, limit, resp.StatusCode, raw)
		}
		resp.Body.Close()
	}
	assertProblemJSON(t, register(limit), http.StatusBadRequest, "TRUSTED_KEY_CAP_REACHED")

	// A key in its grace period still verifies, so it keeps its slot; one
	// invalidated with no grace frees it.
	invalidate := func(i, graceSec int) {
		t.Helper()
		resp := adminRequestAs(t, clientID, secret, "POST", fmt.Sprintf("/oauth/keys/trusted/%s-%d/invalidate", tenant, i),
			mustJSON(t, map[string]any{"gracePeriodSec": graceSec}))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("invalidate key %d: status=%d; body: %s", i, resp.StatusCode, raw)
		}
	}
	invalidate(0, 3600)
	assertProblemJSON(t, register(limit), http.StatusBadRequest, "TRUSTED_KEY_CAP_REACHED")
	invalidate(0, 0)
	if resp := register(limit); resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("register after freeing a slot: status=%d; body: %s", resp.StatusCode, raw)
	} else {
		resp.Body.Close()
	}

	// Reactivating a key makes it verify again, so it is held to the cap too.
	resp := adminRequestAs(t, clientID, secret, "POST", fmt.Sprintf("/oauth/keys/trusted/%s-0/reactivate", tenant),
		mustJSON(t, map[string]any{"validTo": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "TRUSTED_KEY_CAP_REACHED")
}

// NOTE: KEY_OWNED_BY_DIFFERENT_TENANT — covered by TestE2E_CrossTenant_TrustedKey_409
// in oauth_keys_test.go (same package).

// ─────────────────────────────────────────────────────────────────────────────
// deleteTrustedKey error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestTrusted_Delete_BadId_400 verifies that a keyId containing characters
// outside the allowed pattern returns 400 BAD_REQUEST.
func TestTrusted_Delete_BadId_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	// '!' is outside ^[A-Za-z0-9._-]{1,128}$ — MatchesTrustedKIDPattern rejects it.
	resp := adminRequest(t, "DELETE", "/oauth/keys/trusted/bad!kid", nil)
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Delete_UnknownId_404 verifies that deleting a valid-format but
// non-existent keyId returns 404 TRUSTED_KEY_NOT_FOUND.
func TestTrusted_Delete_UnknownId_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := adminRequest(t, "DELETE", "/oauth/keys/trusted/valid-but-nonexistent", nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}

// ─────────────────────────────────────────────────────────────────────────────
// invalidateTrustedKey error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestTrusted_Invalidate_BadId_400 verifies that an invalid keyId format
// returns 400 BAD_REQUEST.
func TestTrusted_Invalidate_BadId_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/bad!kid/invalidate", nil)
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Invalidate_UnknownId_404 verifies that invalidating a
// valid-format but non-existent keyId returns 404 TRUSTED_KEY_NOT_FOUND.
func TestTrusted_Invalidate_UnknownId_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/valid-but-nonexistent/invalidate", nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}

// ─────────────────────────────────────────────────────────────────────────────
// reactivateTrustedKey error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestTrusted_Reactivate_BadId_400 verifies that an invalid keyId format
// returns 400 BAD_REQUEST (pattern check runs before validTo validation and
// key-store lookup).
func TestTrusted_Reactivate_BadId_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	body := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/bad!kid/reactivate", body)
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Reactivate_UnknownId_404 verifies that reactivating a
// valid-format but non-existent keyId returns 404 TRUSTED_KEY_NOT_FOUND.
func TestTrusted_Reactivate_UnknownId_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	body := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/valid-but-nonexistent/reactivate", body)
	assertProblemJSON(t, resp, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}
