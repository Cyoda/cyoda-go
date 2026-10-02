package e2e_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/app"
)

// requestAs issues a request to serverURL+"/api"+path with token as bearer.
func requestAs(t *testing.T, token, method, path string, body []byte) *http.Response {
	t.Helper()
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

// adminRequest issues a request as the suite's tenant admin.
func adminRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	return requestAs(t, suiteToken(t), method, path, body)
}

// operatorRequest issues a request as a platform operator: the platform-wide
// admin endpoints (key pairs, /admin/*) accept only this.
func operatorRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	return requestAs(t, platformToken(t), method, path, body)
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

// deleteTrustedKeyOnCleanup deletes a trusted key the suite tenant
// registered, when the test ends. The tenant's trusted-key cap is shared by
// every test in the run, so a test must not leave keys behind. A key already
// deleted by the test itself is fine.
func deleteTrustedKeyOnCleanup(t *testing.T, kid string) {
	t.Helper()
	t.Cleanup(func() {
		resp := adminRequest(t, "DELETE", "/oauth/keys/trusted/"+kid, nil)
		resp.Body.Close()
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Happy-path round-trips for the 10 /oauth/keys/* operations
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_IssueJwtKeyPair_Happy(t *testing.T) {
	body := mustJSON(t, map[string]any{"algorithm": "RS256"})
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair", body)
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
	// This key pair's validFrom (now) outranks every earlier signer, so it
	// becomes the server-global signer the instant it is issued; delete it
	// on cleanup so it does not linger as the signer for later tests.
	t.Cleanup(func() {
		del := operatorRequest(t, "DELETE", "/oauth/keys/keypair/"+dto.KeyId, nil)
		del.Body.Close()
	})
	if dto.Algorithm != genapi.JwtKeyPairResponseDtoAlgorithmRS256 {
		t.Errorf("expected algorithm RS256, got %s", dto.Algorithm)
	}
	if dto.PublicKey == "" {
		t.Error("expected non-empty publicKey in response")
	}
}

func TestE2E_GetCurrentJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair first so there is an active one.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256"})
	issueResp := operatorRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	issueRaw, _ := io.ReadAll(issueResp.Body)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue prerequisite keypair: got %d", issueResp.StatusCode)
	}
	var issued genapi.JwtKeyPairResponseDto
	_ = json.Unmarshal(issueRaw, &issued)
	// This key pair's validFrom (now) outranks every earlier signer, so it
	// becomes the server-global signer the instant it is issued; delete it
	// on cleanup so it does not linger as the signer for later tests.
	t.Cleanup(func() {
		del := operatorRequest(t, "DELETE", "/oauth/keys/keypair/"+issued.KeyId, nil)
		del.Body.Close()
	})

	resp := operatorRequest(t, "GET", "/oauth/keys/keypair/current", nil)
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

// TestE2E_KeyPairIssuedAheadDoesNotSignYet: a key pair issued with a future
// validFrom is not used to sign until its window opens. Tokens issued now are
// signed by a key already in its window, and they authenticate.
func TestE2E_KeyPairIssuedAheadDoesNotSignYet(t *testing.T) {
	from := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair",
		mustJSON(t, map[string]any{"algorithm": "RS256", "validFrom": from}))
	var issued genapi.JwtKeyPairResponseDto
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue: status=%d; body: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &issued); err != nil || issued.KeyId == "" {
		t.Fatalf("issue: no keyId in %s (err %v)", raw, err)
	}
	// Other tests may fetch a fresh token signed by the server-global
	// signer; remove the key so it never becomes a candidate.
	t.Cleanup(func() {
		del := operatorRequest(t, "DELETE", "/oauth/keys/keypair/"+issued.KeyId, nil)
		del.Body.Close()
	})

	token := suiteToken(t)
	header, err := base64.RawURLEncoding.DecodeString(strings.SplitN(token, ".", 2)[0])
	if err != nil {
		t.Fatalf("decode token header: %v", err)
	}
	var h struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(header, &h); err != nil {
		t.Fatalf("parse token header: %v", err)
	}
	if h.Kid == issued.KeyId {
		t.Fatalf("token signed with key %s, whose window opens at %s", h.Kid, from)
	}

	use := unauthRequest(t, http.MethodGet, "/api/model/", "Bearer "+token)
	defer use.Body.Close()
	if use.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(use.Body)
		t.Fatalf("token signed with the current key was rejected; body: %s", body)
	}
}

// TestE2E_IssueJwtKeyPair_FutureValidFromWithInvalidateCurrent_400: issuing a
// key pair ahead of time together with invalidateCurrent would leave no
// signing key until the new window opens, so it is refused.
func TestE2E_IssueJwtKeyPair_FutureValidFromWithInvalidateCurrent_400(t *testing.T) {
	from := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair", mustJSON(t, map[string]any{
		"algorithm": "RS256", "validFrom": from, "invalidateCurrent": true,
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")

	// The current key still signs: a token can be issued and used.
	use := unauthRequest(t, http.MethodGet, "/api/model/", "Bearer "+suiteToken(t))
	defer use.Body.Close()
	if use.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(use.Body)
		t.Fatalf("the refused request disturbed the current key; body: %s", body)
	}
}

// TestE2E_IssueJwtKeyPair_ValidToInPast_400: a key pair whose window has
// already ended could never sign, so issuing one is refused.
func TestE2E_IssueJwtKeyPair_ValidToInPast_400(t *testing.T) {
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair", mustJSON(t, map[string]any{
		"algorithm": "RS256",
		"validFrom": "2020-01-01T00:00:00Z", "validTo": "2020-01-02T00:00:00Z",
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestE2E_ReactivateJwtKeyPair_FutureValidFrom_400: reactivating the key that
// signs now with a future validFrom would leave no signing key, so it is
// refused, and tokens keep working.
func TestE2E_ReactivateJwtKeyPair_FutureValidFrom_400(t *testing.T) {
	cur := operatorRequest(t, "GET", "/oauth/keys/keypair/current", nil)
	var current genapi.JwtKeyPairResponseDto
	raw, _ := io.ReadAll(cur.Body)
	cur.Body.Close()
	if cur.StatusCode != http.StatusOK || json.Unmarshal(raw, &current) != nil || current.KeyId == "" {
		t.Fatalf("current key: status=%d body=%s", cur.StatusCode, raw)
	}

	from := time.Now().Add(time.Hour).UTC()
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+current.KeyId+"/reactivate", mustJSON(t, map[string]any{
		"validFrom": from.Format(time.RFC3339), "validTo": from.Add(24 * time.Hour).Format(time.RFC3339),
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")

	use := unauthRequest(t, http.MethodGet, "/api/model/", "Bearer "+suiteToken(t))
	defer use.Body.Close()
	if use.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(use.Body)
		t.Fatalf("the refused request disturbed the current key; body: %s", body)
	}
}

func TestE2E_DeleteJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair to delete.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256"})
	issueResp := operatorRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	resp := operatorRequest(t, "DELETE", "/oauth/keys/keypair/"+issued.KeyId, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_InvalidateJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair to invalidate.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256"})
	issueResp := operatorRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+issued.KeyId+"/invalidate", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_ReactivateJwtKeyPair_Happy(t *testing.T) {
	// Issue then invalidate then reactivate.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256"})
	issueResp := operatorRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	invResp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+issued.KeyId+"/invalidate", nil)
	invResp.Body.Close()
	if invResp.StatusCode != http.StatusOK {
		t.Fatalf("invalidate: got %d", invResp.StatusCode)
	}

	reactivateBody := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+issued.KeyId+"/reactivate", reactivateBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

// TestE2E_KeyPair_NoAudience: /current takes no audience query parameter,
// and a key pair issued with a stray "audience":"human" field in the body is
// accepted — the field is unknown to the server and has no effect, since key
// pairs have no audience — and its validFrom (now) makes it the one
// /current returns. No invalidateCurrent here: nothing pre-existing is
// invalidated, so cleanup only has to delete the one key pair this test
// created.
func TestE2E_KeyPair_NoAudience(t *testing.T) {
	resp := operatorRequest(t, http.MethodPost, "/oauth/keys/keypair",
		mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "human"}))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue: %d %s", resp.StatusCode, body)
	}
	var issued struct {
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal([]byte(body), &issued); err != nil || issued.KeyID == "" {
		t.Fatalf("issue: no keyId in %s (decode error: %v)", body, err)
	}
	t.Cleanup(func() {
		del := operatorRequest(t, http.MethodDelete, "/oauth/keys/keypair/"+issued.KeyID, nil)
		del.Body.Close()
	})

	resp = operatorRequest(t, http.MethodGet, "/oauth/keys/keypair/current", nil)
	cur := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(cur, issued.KeyID) {
		t.Fatalf("current: %d %s; want key %s", resp.StatusCode, cur, issued.KeyID)
	}
}

func TestE2E_RegisterTrustedKey_Happy(t *testing.T) {
	kid := fmt.Sprintf("e2e-tk-%d", time.Now().UnixNano())
	deleteTrustedKeyOnCleanup(t, kid)
	body := mustJSON(t, map[string]any{
		"keyId": kid,
		"jwk":   rsaJWK(t, kid),
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
	deleteTrustedKeyOnCleanup(t, kid)
	regBody := mustJSON(t, map[string]any{
		"keyId": kid,
		"jwk":   rsaJWK(t, kid),
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
		"keyId": kid,
		"jwk":   rsaJWK(t, kid),
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
		"keyId": kid,
		"jwk":   rsaJWK(t, kid),
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
	deleteTrustedKeyOnCleanup(t, kid)
	regBody := mustJSON(t, map[string]any{
		"keyId": kid,
		"jwk":   rsaJWK(t, kid),
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
// from the published set and therefore absent from JWKS.
func TestE2E_GracePeriodRoundTrip(t *testing.T) {
	// Step 1: issue keypair A.
	bodyA := mustJSON(t, map[string]any{"algorithm": "RS256"})
	respA := operatorRequest(t, "POST", "/oauth/keys/keypair", bodyA)
	var kpA genapi.JwtKeyPairResponseDto
	json.NewDecoder(respA.Body).Decode(&kpA)
	respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("issue A: got %d", respA.StatusCode)
	}

	// Step 2: issue keypair B with invalidateCurrent=true and a 2 s grace period.
	bodyB := mustJSON(t, map[string]any{
		"algorithm":                "RS256",
		"invalidateCurrent":        true,
		"invalidateGracePeriodSec": int64(2),
	})
	respB := operatorRequest(t, "POST", "/oauth/keys/keypair", bodyB)
	var kpB genapi.JwtKeyPairResponseDto
	json.NewDecoder(respB.Body).Decode(&kpB)
	respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("issue B: got %d", respB.StatusCode)
	}

	// Step 3: immediately after rotation, BOTH A (grace-period) and B (active)
	// appear in JWKS. A's ValidTo is still in the future so the published set
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
	oversized := fmt.Sprintf(`{"algorithm":"RS256","_padding":"%s"}`, padding)

	token := platformToken(t)
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
	oversized := fmt.Sprintf(`{"keyId":"e2e-size","_padding":"%s"}`, padding)

	token := suiteToken(t)
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

// adminTokenForTenant signs a self-contained M2M-shaped admin token
// (scopes ROLE_ADMIN,ROLE_M2M) for an arbitrary tenant/user, directly with
// the shared server's own signing key. POST /clients derives a new client's
// tenant from the caller's own claims, so createM2MClient uses this to seed a
// client belonging to tenantID without reaching into the store.
func adminTokenForTenant(t *testing.T, tenant, user string) string {
	t.Helper()
	tok, err := signServiceToken(e2eSignKey, e2eIssuer, "", user, tenant, user, []string{"ROLE_ADMIN", "ROLE_M2M"})
	if err != nil {
		t.Fatalf("sign admin token: %v", err)
	}
	return tok
}

// createM2MClient provisions a new M2M client belonging to tenantID, through
// POST /clients authenticated with a seed admin token minted for that tenant
// and seedUser (adminTokenForTenant) rather than reaching into the store
// directly. withAdmin asks for ROLE_ADMIN (?withAdminRole=true); the client's
// roles and user id are what POST /clients assigns, not chosen here.
// Returns (clientID, clientSecret). The client is deleted when the test ends.
func createM2MClient(t *testing.T, tenantID, seedUser string, withAdmin bool) (string, string) {
	t.Helper()
	seed := adminTokenForTenant(t, tenantID, seedUser)
	path := "/clients"
	if withAdmin {
		path += "?withAdminRole=true"
	}
	resp := unauthRequest(t, http.MethodPost, "/api"+path, "Bearer "+seed)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createM2MClient: seed POST /clients: %d: %s", resp.StatusCode, raw)
	}
	cred := decodeCredential(t, "createM2MClient", raw)
	deleteClientAtCleanup(t, serverURL, cred.id, func() string { return adminTokenForTenant(t, tenantID, seedUser) })
	return cred.id, cred.secret
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

// TestE2E_TrustedKey_SameKidInTwoTenants: key ids are unique per tenant. Two
// tenants register different keys under one kid; both registrations succeed,
// and each tenant lists exactly its own key.
func TestE2E_TrustedKey_SameKidInTwoTenants(t *testing.T) {
	kid := "same-kid-" + uuid.NewString()[:8]
	tokA, tokB := suiteToken(t), adminTokenForTenant(t, "tenant-kid-b", "admin-b")
	for _, tok := range []string{tokA, tokB} {
		resp := requestAs(t, tok, http.MethodPost, "/oauth/keys/trusted",
			mustJSON(t, map[string]any{"keyId": kid, "jwk": rsaJWK(t, kid)}))
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			t.Fatalf("register: %d %s", resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}
	t.Cleanup(func() {
		for _, tok := range []string{tokA, tokB} {
			requestAs(t, tok, http.MethodDelete, "/oauth/keys/trusted/"+kid, nil).Body.Close()
		}
	})
	for _, tok := range []string{tokA, tokB} {
		resp := requestAs(t, tok, http.MethodGet, "/oauth/keys/trusted", nil)
		var keys []map[string]any
		_ = json.Unmarshal([]byte(readBody(t, resp)), &keys)
		n := 0
		for _, k := range keys {
			if k["keyId"] == kid {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("tenant sees %d keys with kid %s, want exactly its own", n, kid)
		}
	}
}

// NOTE: E2E feature-flag coverage — the server is started once in TestMain
// with TrustedKeyRegistrationEnabled=true. There is no mechanism to restart
// the server with the flag flipped to false for a single test within the
// TestMain harness. Adapter-level TestRegisterTrustedKey_FlagDisabled_404
// covers the invariant at handler level, which is where the flag is enforced.

// ─────────────────────────────────────────────────────────────────────────────
// issueJwtKeyPair error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestKeys_IssueNonRS256_400UnsupportedAlgorithm verifies that requesting a
// non-RS256 algorithm returns 400 UNSUPPORTED_ALGORITHM.
func TestKeys_IssueNonRS256_400UnsupportedAlgorithm(t *testing.T) {
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair", mustJSON(t, map[string]any{
		"algorithm": "ES256",
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "UNSUPPORTED_ALGORITHM")
}

// unknownKeyPairID is a well-formed key-pair id (32 lowercase hex) that no
// store issues or derives.
const unknownKeyPairID = "00000000000000000000000000000000"

// ─────────────────────────────────────────────────────────────────────────────
// keyId format and unknown keyId (delete, invalidate, reactivate)
// ─────────────────────────────────────────────────────────────────────────────

// TestKeys_KeyPair_MalformedId_400 verifies that a keyId that is not 32
// lowercase hex characters returns 400 BAD_REQUEST on every key-pair
// lifecycle endpoint.
func TestKeys_KeyPair_MalformedId_400(t *testing.T) {
	validTo := mustJSON(t, map[string]any{"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339)})
	for _, c := range []struct {
		method, path string
		body         []byte
	}{
		{"DELETE", "/oauth/keys/keypair/not-a-kid", nil},
		{"POST", "/oauth/keys/keypair/not-a-kid/invalidate", nil},
		{"POST", "/oauth/keys/keypair/not-a-kid/reactivate", validTo},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			assertProblemJSON(t, operatorRequest(t, c.method, c.path, c.body), http.StatusBadRequest, "BAD_REQUEST")
		})
	}
}

// TestKeys_DeleteKeyPair_UnknownId_404 verifies that deleting a well-formed
// but unknown keyId returns 404 KEYPAIR_NOT_FOUND.
func TestKeys_DeleteKeyPair_UnknownId_404(t *testing.T) {
	resp := operatorRequest(t, "DELETE", "/oauth/keys/keypair/"+unknownKeyPairID, nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "KEYPAIR_NOT_FOUND")
}

// TestKeys_InvalidateKeyPair_BadGrace_400 verifies that gracePeriodSec < 0
// returns 400 BAD_REQUEST. The grace check runs before the key-store lookup,
// so a well-formed unknown keyId triggers it.
func TestKeys_InvalidateKeyPair_BadGrace_400(t *testing.T) {
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+unknownKeyPairID+"/invalidate", mustJSON(t, map[string]any{
		"gracePeriodSec": int64(-1),
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestKeys_InvalidateKeyPair_UnknownId_404 verifies that invalidating a
// well-formed but unknown keyId returns 404 KEYPAIR_NOT_FOUND.
func TestKeys_InvalidateKeyPair_UnknownId_404(t *testing.T) {
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+unknownKeyPairID+"/invalidate", nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "KEYPAIR_NOT_FOUND")
}

// TestKeys_ReactivateKeyPair_BadBody_400 verifies that omitting the required
// validTo field returns 400 BAD_REQUEST. The validTo check runs before the
// key-store lookup, so a well-formed unknown keyId triggers it.
func TestKeys_ReactivateKeyPair_BadBody_400(t *testing.T) {
	// {} decodes to zero ValidTo; handler returns 400 "validTo required".
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+unknownKeyPairID+"/reactivate", mustJSON(t, map[string]any{}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestKeys_ReactivateKeyPair_UnknownId_404 verifies that reactivating a
// well-formed but unknown keyId returns 404 KEYPAIR_NOT_FOUND.
func TestKeys_ReactivateKeyPair_UnknownId_404(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+unknownKeyPairID+"/reactivate", body)
	assertProblemJSON(t, resp, http.StatusNotFound, "KEYPAIR_NOT_FOUND")
}

// ─────────────────────────────────────────────────────────────────────────────
// registerTrustedKey error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestTrusted_RegisterNonRSA_400UnsupportedKeyType verifies that submitting a
// non-RSA JWK (EC kty) returns 400 UNSUPPORTED_KEY_TYPE.
func TestTrusted_RegisterNonRSA_400UnsupportedKeyType(t *testing.T) {
	// Minimal EC JWK — the server accepts only RSA.
	resp := adminRequest(t, "POST", "/oauth/keys/trusted", mustJSON(t, map[string]any{
		"keyId": "e2e-ec-type-test",
		"jwk":   map[string]any{"kty": "EC"},
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "UNSUPPORTED_KEY_TYPE")
}

// TestTrustedKey_CapReached_400: registering one key past the per-tenant cap
// (CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT) is 400 TRUSTED_KEY_CAP_REACHED. It
// runs in a tenant of its own, so filling that tenant's cap does not affect
// any other test. Invalidating a key ends it at once, so it frees its slot at
// once; reactivating it makes it verify again, so it is held to the cap.
func TestTrustedKey_CapReached_400(t *testing.T) {
	tenant := fmt.Sprintf("e2e-cap-%d", time.Now().UnixNano())
	clientID, secret := createM2MClient(t, tenant, "cap-admin", true)
	limit := app.DefaultConfig().IAM.TrustedKeyMaxPerTenant

	register := func(i int) *http.Response {
		kid := fmt.Sprintf("%s-%d", tenant, i)
		return adminRequestAs(t, clientID, secret, "POST", "/oauth/keys/trusted",
			mustJSON(t, map[string]any{"keyId": kid, "jwk": rsaJWK(t, kid)}))
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

	inv := adminRequestAs(t, clientID, secret, "POST", fmt.Sprintf("/oauth/keys/trusted/%s-0/invalidate", tenant), nil)
	if inv.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(inv.Body)
		inv.Body.Close()
		t.Fatalf("invalidate key 0: status=%d; body: %s", inv.StatusCode, raw)
	}
	inv.Body.Close()
	if resp := register(limit); resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("register after freeing a slot: status=%d; body: %s", resp.StatusCode, raw)
	} else {
		resp.Body.Close()
	}

	resp := adminRequestAs(t, clientID, secret, "POST", fmt.Sprintf("/oauth/keys/trusted/%s-0/reactivate", tenant),
		mustJSON(t, map[string]any{"validTo": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "TRUSTED_KEY_CAP_REACHED")
}

// ─────────────────────────────────────────────────────────────────────────────
// deleteTrustedKey / invalidateTrustedKey / reactivateTrustedKey error surface
// ─────────────────────────────────────────────────────────────────────────────

// TestTrusted_Delete_BadId_400 verifies that a keyId containing characters
// outside the allowed pattern returns 400 BAD_REQUEST.
func TestTrusted_Delete_BadId_400(t *testing.T) {
	// '!' is outside ^[A-Za-z0-9._-]{1,128}$ — MatchesTrustedKIDPattern rejects it.
	resp := adminRequest(t, "DELETE", "/oauth/keys/trusted/bad!kid", nil)
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Delete_UnknownId_404 verifies that deleting a valid-format but
// non-existent keyId returns 404 TRUSTED_KEY_NOT_FOUND.
func TestTrusted_Delete_UnknownId_404(t *testing.T) {
	resp := adminRequest(t, "DELETE", "/oauth/keys/trusted/valid-but-nonexistent", nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}

// TestTrusted_Invalidate_BadId_400 verifies that an invalid keyId format
// returns 400 BAD_REQUEST.
func TestTrusted_Invalidate_BadId_400(t *testing.T) {
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/bad!kid/invalidate", nil)
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Invalidate_UnknownId_404 verifies that invalidating a
// valid-format but non-existent keyId returns 404 TRUSTED_KEY_NOT_FOUND.
func TestTrusted_Invalidate_UnknownId_404(t *testing.T) {
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/valid-but-nonexistent/invalidate", nil)
	assertProblemJSON(t, resp, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}

// TestTrusted_Reactivate_BadId_400 verifies that an invalid keyId format
// returns 400 BAD_REQUEST (pattern check runs before validTo validation and
// key-store lookup).
func TestTrusted_Reactivate_BadId_400(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/bad!kid/reactivate", body)
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Reactivate_UnknownId_404 verifies that reactivating a
// valid-format but non-existent keyId returns 404 TRUSTED_KEY_NOT_FOUND.
func TestTrusted_Reactivate_UnknownId_404(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"validTo": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/valid-but-nonexistent/reactivate", body)
	assertProblemJSON(t, resp, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}

// ─────────────────────────────────────────────────────────────────────────────
// Timestamps a key record cannot hold
// ─────────────────────────────────────────────────────────────────────────────

// year10000 is a timestamp Go reads whose UTC form is in year 10000: a key
// record holding it could never be read back.
const year10000 = "9999-12-31T23:59:59-05:00"

// TestKeys_IssueKeyPair_ValidToOutOfRange_400 verifies that a validTo whose
// UTC year is outside 1..9999 returns 400 BAD_REQUEST.
func TestKeys_IssueKeyPair_ValidToOutOfRange_400(t *testing.T) {
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair", mustJSON(t, map[string]any{
		"algorithm": "RS256",
		"validTo":   year10000,
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestKeys_ReactivateKeyPair_ValidToOutOfRange_400: the range check runs
// before the key-store lookup, so a well-formed unknown keyId triggers it.
func TestKeys_ReactivateKeyPair_ValidToOutOfRange_400(t *testing.T) {
	resp := operatorRequest(t, "POST", "/oauth/keys/keypair/"+unknownKeyPairID+"/reactivate", mustJSON(t, map[string]any{
		"validTo": year10000,
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}

// TestTrusted_Register_ValidToOutOfRange_400 verifies that registering a
// trusted key with a validTo whose UTC year is outside 1..9999 returns 400
// BAD_REQUEST and stores nothing.
func TestTrusted_Register_ValidToOutOfRange_400(t *testing.T) {
	kid := fmt.Sprintf("e2e-far-%d", time.Now().UnixNano())
	resp := adminRequest(t, "POST", "/oauth/keys/trusted", mustJSON(t, map[string]any{
		"keyId":   kid,
		"jwk":     rsaJWK(t, kid),
		"validTo": year10000,
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
	del := adminRequest(t, "DELETE", "/oauth/keys/trusted/"+kid, nil)
	assertProblemJSON(t, del, http.StatusNotFound, "TRUSTED_KEY_NOT_FOUND")
}

// TestTrusted_Reactivate_ValidToOutOfRange_400: the range check runs before
// the key-store lookup, so a well-formed unknown keyId triggers it.
func TestTrusted_Reactivate_ValidToOutOfRange_400(t *testing.T) {
	resp := adminRequest(t, "POST", "/oauth/keys/trusted/valid-but-nonexistent/reactivate", mustJSON(t, map[string]any{
		"validTo": year10000,
	}))
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}
