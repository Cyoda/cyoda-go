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
	"testing"
	"time"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// adminRequest issues an authenticated request using the bootstrap admin token.
// The path must start with "/" and is appended to serverURL+"/api".
func adminRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	token := getToken(t, "test-client", "test-secret")
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
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, raw)
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
	if issueResp.StatusCode != http.StatusCreated {
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
	if issueResp.StatusCode != http.StatusCreated {
		t.Fatalf("issue: got %d", issueResp.StatusCode)
	}

	resp := adminRequest(t, "DELETE", "/oauth/keys/keypair/"+issued.KeyId, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, raw)
	}
}

func TestE2E_InvalidateJwtKeyPair_Happy(t *testing.T) {
	// Issue a keypair to invalidate.
	issueBody := mustJSON(t, map[string]any{"algorithm": "RS256", "audience": "client"})
	issueResp := adminRequest(t, "POST", "/oauth/keys/keypair", issueBody)
	var issued genapi.JwtKeyPairResponseDto
	json.NewDecoder(issueResp.Body).Decode(&issued)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusCreated {
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
	if issueResp.StatusCode != http.StatusCreated {
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
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, raw)
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
	if regResp.StatusCode != http.StatusCreated {
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
	if regResp.StatusCode != http.StatusCreated {
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
	if regResp.StatusCode != http.StatusCreated {
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
	if regResp.StatusCode != http.StatusCreated {
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
