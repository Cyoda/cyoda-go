package auth_test

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// forgeTokenWithAlg produces a JWT whose header declares the given alg but
// whose body and signature are otherwise as the caller requests. For
// alg:"none" the signature segment is empty. For alg:"HS256" we pretend to
// supply an HMAC (the bytes don't need to be valid — the guard must fire
// before any verification attempt). For "RS256" we sign legitimately.
//
// The purpose is to prove that the verification path pins alg=RS256 at the
// token-header level, not just at the implementation level (which always
// computes SHA256 regardless of header, so forged alg claims would
// otherwise be silently accepted).
func forgeTokenWithAlg(t *testing.T, alg, kid string, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	header := map[string]string{"alg": alg, "typ": "JWT", "kid": kid}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := headerB64 + "." + claimsB64

	var sigB64 string
	switch alg {
	case "none":
		sigB64 = ""
	case "HS256":
		// Fabricated bytes — the guard must reject before checking these.
		sigB64 = base64.RawURLEncoding.EncodeToString([]byte("not-a-real-hmac"))
	default:
		// Legitimately sign with RS256 semantics so an RS256 header passes.
		legit, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return legit
	}
	return signingInput + "." + sigB64
}

// TestJWKSValidator_RejectsAlgNone confirms the `alg: none` attack is
// blocked. This is the canonical JWT downgrade attack: a token with an
// empty signature and alg:none is structurally valid, and a validator
// that trusts `alg` and skips verification would accept it. Our validator
// hardcodes SHA256 verification so it would fail signature check, but a
// header with alg:"none" should be rejected explicitly — before the
// verifier even runs — to match the JWT security best practice.
func TestJWKSValidator_RejectsAlgNone(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	token := forgeTokenWithAlg(t, "none", kid, claims, key)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("validator accepted alg:none token")
	}
	if !strings.Contains(err.Error(), "alg") {
		t.Errorf("expected error mentioning alg, got %v", err)
	}
}

// TestJWKSValidator_RejectsAlgHS256 blocks the algorithm-confusion attack:
// a token whose header claims HS256 (symmetric) must not be accepted by
// a validator configured for RS256 (asymmetric). The classic variant uses
// the RSA public key as the HMAC key; we don't even need to construct
// that here because alg-pinning rejects the header long before.
func TestJWKSValidator_RejectsAlgHS256(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	token := forgeTokenWithAlg(t, "HS256", kid, claims, key)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("validator accepted alg:HS256 token")
	}
	if !strings.Contains(err.Error(), "alg") {
		t.Errorf("expected error mentioning alg, got %v", err)
	}
}

// TestJWKSValidator_RejectsMissingAlg blocks header tampering where the
// alg claim is stripped entirely — equally dangerous because some naive
// validators treat missing alg as permission to skip checks.
func TestJWKSValidator_RejectsMissingAlg(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	// Hand-build a token with no alg field at all.
	header := map[string]string{"typ": "JWT", "kid": kid}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	token := fmt.Sprintf("%s.%s.",
		base64.RawURLEncoding.EncodeToString(headerJSON),
		base64.RawURLEncoding.EncodeToString(claimsJSON),
	)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("validator accepted token with no alg header")
	}
	if !strings.Contains(err.Error(), "alg") {
		t.Errorf("expected error mentioning alg, got %v", err)
	}
}

// TestJWKSValidator_AcceptsAlgRS256 is the happy path — alg pinning must
// not regress the expected case.
func TestJWKSValidator_AcceptsAlgRS256(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	token := forgeTokenWithAlg(t, "RS256", kid, claims, key)
	if _, err := v.Validate(token); err != nil {
		t.Fatalf("validator rejected legitimate RS256 token: %v", err)
	}
}
