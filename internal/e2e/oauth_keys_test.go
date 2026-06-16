package e2e_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// adminRequest issues an authenticated request using the bootstrap admin token.
// The path must start with "/" and is appended to serverURL+"/api".
func adminRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	token := getToken(t, "testclient", "testsecret")
	var br io.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	req, err := e2eNewRequest(t, method, serverURL+"/api"+path, br)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}

// rsaJWK builds a minimal public-key JWK from a freshly generated RSA key.
func rsaJWK(t *testing.T, kid string) map[string]any {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	return map[string]any{"kty": "RSA", "kid": kid, "n": n, "e": e}
}

// mustJSON marshals v or calls t.Fatal.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ─────────────────────────────────────────────────────────────────────────────
// Happy-path round-trips for the 10 /oauth/keys/* operations
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_IssueJwtKeyPair_Happy(t *testing.T) {
	body := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	resp := adminRequest(t, "POST", "/oauth/keys/keypair", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var dto genapi.JwtKeyPairResponseDto
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if dto.KeyId == "" {
		t.Fatal("expected non-empty keyId in response")
	}
	if dto.Algorithm != genapi.JwtKeyPairResponseDtoAlgorithmRS256 {
		t.Errorf("expected algorithm RS256, got %s", dto.Algorithm)
	}
	if dto.PublicKey == "" {
		t.Error("expected non-empty publicKey in response")
	}
}

func TestE2E_GetCurrentJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair first so there is an active one.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "human"})
	issueResp := adminRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue prerequisite keypair: got %d", issueResp.StatusCode)
	}

	resp := adminRequest(t, "GET", "/oauth/keys/keypair/current?audience=human", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var dto genapi.JwtKeyPairResponseDto
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if dto.KeyId == "" {
		t.Fatal("expected non-empty keyId")
	}
}

func TestE2E_DeleteJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair to delete.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	issueResp := adminRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	resp := adminRequest(t, "DELETE", "/oauth/keys/keypair/"+issued.KeyId, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_InvalidateJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair to invalidate.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	issueResp := adminRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	resp := adminRequest(t, "POST", "/oauth/keys/keypair/"+issued.KeyId+"/invalidate", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_ReactivateJwtKeyPair_Happy(t *testing.T) {
	// Issue then invalidate then reactivate.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	issueResp := adminRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	invResp := adminRequest(t, "POST", "/oauth/keys/keypair/"+issued.KeyId+"/invalidate", nil)
	invResp.Body.Close()
	if invResp.StatusCode != http.StatusOK {
		t.Fatalf("invalidate: got %d", invResp.StatusCode)
	}

	reactivateBody := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/keypair/"+issued.KeyId+"/reactivate", reactivateBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_RegisterTrustedKey_Happy(t *testing.T) {
	kid := fmt.Sprintf("e2e-tk-%d", time.Now().UnixNano())
	body := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "human",
	})
	resp := adminRequest(t, "POST", "/oauth/keys/trusted", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var dto genapi.TrustedKeyResponseDto
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if dto.KeyId != kid {
		t.Errorf("expected keyId %q, got %q", kid, dto.KeyId)
	}
	if dto.LegalEntityId == "" {
		t.Error("expected non-empty legalEntityId")
	}
}

func TestE2E_ListTrustedKeys_Happy(t *testing.T) {
	// Register at least one key so the list is non-empty.
	kid := fmt.Sprintf("e2e-list-%d", time.Now().UnixNano())
	regBody := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "client",
	})
	regResp := adminRequest(t, "POST", "/oauth/keys/trusted", regBody)
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("register prerequisite: got %d", regResp.StatusCode)
	}

	resp := adminRequest(t, "GET", "/oauth/keys/trusted", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var keys []genapi.TrustedKeyResponseDto
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.KeyId == kid {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("registered key %q not found in list response", kid)
	}
}

func TestE2E_DeleteTrustedKey_Happy(t *testing.T) {
	kid := fmt.Sprintf("e2e-del-%d", time.Now().UnixNano())
	regBody := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "client",
	})
	regResp := adminRequest(t, "POST", "/oauth/keys/trusted", regBody)
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("register: got %d", regResp.StatusCode)
	}

	resp := adminRequest(t, "DELETE", "/oauth/keys/trusted/"+kid, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_InvalidateTrustedKey_Happy(t *testing.T) {
	kid := fmt.Sprintf("e2e-inv-%d", time.Now().UnixNano())
	regBody := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "human",
	})
	regResp := adminRequest(t, "POST", "/oauth/keys/trusted", regBody)
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("register: got %d", regResp.StatusCode)
	}

	resp := adminRequest(t, "POST", "/oauth/keys/trusted/"+kid+"/invalidate", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_ReactivateTrustedKey_Happy(t *testing.T) {
	kid := fmt.Sprintf("e2e-react-%d", time.Now().UnixNano())
	regBody := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "human",
	})
	regResp := adminRequest(t, "POST", "/oauth/keys/trusted", regBody)
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("register: got %d", regResp.StatusCode)
	}

	invResp := adminRequest(t, "POST", "/oauth/keys/trusted/"+kid+"/invalidate", nil)
	invResp.Body.Close()
	if invResp.StatusCode != http.StatusOK {
		t.Fatalf("invalidate: got %d", invResp.StatusCode)
	}

	reactivateBody := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/"+kid+"/reactivate", reactivateBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Grace-period round-trip + body-size assertions
// ─────────────────────────────────────────────────────────────────────────────

// fetchJWKSKIDs fetches /.well-known/jwks.json and returns the set of key IDs.
// The JWKS endpoint is mounted under the context path (/api).
func fetchJWKSKIDs(t *testing.T) map[string]bool {
	t.Helper()
	resp, err := http.Get(serverURL + "/api/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("GET jwks.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jwks.json: got %d", resp.StatusCode)
	}
	var jwks struct {
		Keys []struct {
			KID string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	kids := make(map[string]bool, len(jwks.Keys))
	for _, k := range jwks.Keys {
		kids[k.KID] = true
	}
	return kids
}

// TestE2E_GracePeriodRoundTrip issues keypair A, then keypair B with
// invalidateCurrent + a 2 s grace. Asserts the correct JWKS state before and
// after the grace window.
//
// JWKS semantics (spec §3.2 #1): grace-period keys (Active=false, ValidTo in
// the future) ARE published in JWKS so that external verifiers can validate
// tokens signed before the rotation. After ValidTo passes the key is excluded
// by ListForVerification and therefore absent from JWKS.
func TestE2E_GracePeriodRoundTrip(t *testing.T) {
	// Step 1: issue keypair A.
	bodyA := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	respA := adminRequest(t, "POST", "/oauth/keys/keypair", bodyA)
	var kpA genapi.JwtKeyPairResponseDto
	json.NewDecoder(respA.Body).Decode(&kpA)
	respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("issue A: got %d", respA.StatusCode)
	}

	// Step 2: issue keypair B with invalidateCurrent=true and a 2 s grace period.
	bodyB := mustJSON(t, map[string]any{
		"algorithm":                "RS256",
		"audience":                 "client",
		"invalidateCurrent":        true,
		"invalidateGracePeriodSec": int64(2),
	})
	respB := adminRequest(t, "POST", "/oauth/keys/keypair", bodyB)
	var kpB genapi.JwtKeyPairResponseDto
	json.NewDecoder(respB.Body).Decode(&kpB)
	respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("issue B: got %d", respB.StatusCode)
	}

	// Step 3: immediately after rotation, BOTH A (grace-period) and B (active)
	// appear in JWKS. A's ValidTo is still in the future so ListForVerification
	// includes it; JWKS publishes it so external verifiers can validate
	// tokens signed with A before the rotation.
	kidsDuring := fetchJWKSKIDs(t)
	if !kidsDuring[kpA.KeyId] {
		t.Errorf("expected kid A (%s) present in JWKS during grace window; got keys: %v", kpA.KeyId, kidsDuring)
	}
	if !kidsDuring[kpB.KeyId] {
		t.Errorf("expected kid B (%s) present in JWKS as new active key; got keys: %v", kpB.KeyId, kidsDuring)
	}

	// Step 4: wait for grace to expire — A's ValidTo passes; only B remains.
	time.Sleep(3 * time.Second)

	kidsAfter := fetchJWKSKIDs(t)
	if kidsAfter[kpA.KeyId] {
		t.Errorf("expected kid A (%s) absent from JWKS after grace expired; got keys: %v", kpA.KeyId, kidsAfter)
	}
	if !kidsAfter[kpB.KeyId] {
		t.Errorf("expected kid B (%s) still present in JWKS after grace expired; got keys: %v", kpB.KeyId, kidsAfter)
	}
}

// TestE2E_KeypairBodySizeLimit verifies that POST /oauth/keys/keypair rejects
// a request body larger than 1 MiB with 400.
func TestE2E_KeypairBodySizeLimit(t *testing.T) {
	padding := strings.Repeat("x", 1<<20+1)
	oversized := fmt.Sprintf(`{"algorithm":"RS256","audience":"client","_padding":"%s"}`, padding)

	token := getToken(t, "testclient", "testsecret")
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/keys/keypair", strings.NewReader(oversized))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for oversized body, got %d: %s", resp.StatusCode, raw)
	}
}

// TestE2E_TrustedKeyBodySizeLimit verifies that POST /oauth/keys/trusted rejects
// a request body larger than 1 MiB with 400.
func TestE2E_TrustedKeyBodySizeLimit(t *testing.T) {
	padding := strings.Repeat("x", 1<<20+1)
	oversized := fmt.Sprintf(`{"keyId":"e2e-size","audience":"human","_padding":"%s"}`, padding)

	token := getToken(t, "testclient", "testsecret")
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/keys/trusted", strings.NewReader(oversized))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for oversized body, got %d: %s", resp.StatusCode, raw)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Cross-tenant isolation + deferred cases
// ─────────────────────────────────────────────────────────────────────────────

// createM2MClient provisions a new M2M client by reaching into the M2M store
// directly. The legacy POST /api/account/m2m HTTP surface was retired in favour
// of /clients; /clients derives tenant from the caller's auth context, so seeding
// cross-tenant clients requires bypassing HTTP.
// Returns (clientID, clientSecret).
func createM2MClient(t *testing.T, tenantID, userID string, roles []string) (string, string) {
	t.Helper()
	authSvc := testApp.AuthService()
	if authSvc == nil {
		t.Fatal("createM2MClient requires JWT IAM mode (testApp.AuthService() returned nil)")
	}

	// Generate clientID + secret with crypto/rand so the test creates a unique
	// record per call (mirrors the 16-char alphanumeric shape used by /clients).
	randBytes := make([]byte, 12)
	if _, err := rand.Read(randBytes); err != nil {
		t.Fatalf("rand: %v", err)
	}
	clientID := "e2etest" + hex.EncodeToString(randBytes[:6])
	clientSecret := hex.EncodeToString(randBytes[6:])

	if err := authSvc.M2MClientStore().CreateWithSecret(
		clientID,
		spi.TenantID(tenantID),
		userID,
		clientSecret,
		roles,
	); err != nil {
		t.Fatalf("M2MClientStore.CreateWithSecret: %v", err)
	}
	return clientID, clientSecret
}

// adminRequestAs issues an authenticated request using a specific M2M client's token.
func adminRequestAs(t *testing.T, clientID, clientSecret, method, path string, body []byte) *http.Response {
	t.Helper()
	token := getToken(t, clientID, clientSecret)
	var br io.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	req, err := e2eNewRequest(t, method, serverURL+"/api"+path, br)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}

// TestE2E_CrossTenant_TrustedKey_409 registers a trusted key as the bootstrap
// tenant, then attempts to register the same keyId from a second tenant and
// expects 409 KEY_OWNED_BY_DIFFERENT_TENANT. Adapter-level coverage also
// exists at TestRegisterTrustedKey_CrossTenantCollision_409.
func TestE2E_CrossTenant_TrustedKey_409(t *testing.T) {
	// Provision a second M2M client at a different tenant.
	clientBID, clientBSecret := createM2MClient(t, "tenant-b", "user-b", []string{"ROLE_ADMIN", "ROLE_M2M"})

	kid := fmt.Sprintf("e2e-xtenant-%d", time.Now().UnixNano())

	// Register the key as the bootstrap tenant (tenant A = "test-tenant").
	bodyA := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "human",
	})
	respA := adminRequest(t, "POST", "/oauth/keys/trusted", bodyA)
	respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("register as tenant A: got %d", respA.StatusCode)
	}

	// Attempt to register the same keyId from tenant B.
	bodyB := mustJSON(t, map[string]any{
		"keyId":    kid,
		"jwk":      rsaJWK(t, kid),
		"audience": "human",
	})
	respB := adminRequestAs(t, clientBID, clientBSecret, "POST", "/oauth/keys/trusted", bodyB)
	raw, _ := io.ReadAll(respB.Body)
	respB.Body.Close()
	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for cross-tenant collision, got %d: %s", respB.StatusCode, raw)
	}
	// The RFC 7807 problem response embeds the error code in properties.errorCode.
	var errBody struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(raw, &errBody)
	if errBody.Properties.ErrorCode != "KEY_OWNED_BY_DIFFERENT_TENANT" {
		t.Errorf("expected error code KEY_OWNED_BY_DIFFERENT_TENANT, got %q (body: %s)", errBody.Properties.ErrorCode, raw)
	}
}

// NOTE: E2E feature-flag coverage — the server is started once in TestMain
// with TrustedKeyRegistrationEnabled=true. There is no mechanism to restart
// the server with the flag flipped to false for a single test within the
// TestMain harness. Adapter-level TestRegisterTrustedKey_FlagDisabled_404
// covers the invariant at handler level, which is where the flag is enforced.

// NOTE: E2E token-exchange coverage — verifying the token-exchange grant via
// a trusted key requires signing a subject_token with the private key material
// that was used to build the registered JWK. The E2E harness does not retain
// private keys after registration; fabricating a valid signed token in-test
// would duplicate the signing logic. The token-exchange principal-tenant
// invariant is asserted at unit level in internal/auth/store_test.go and
// internal/auth/kv_trusted_store_test.go.
// TODO(#288): add an E2E token-exchange test once the harness supports
// embedded fixture keys with private-key material retained across calls.
