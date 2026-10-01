package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// Regression test: if a KeySource's lookup were keyed on `kid` alone across
// issuers, then two issuers advertising different keys under the same `kid`
// could confuse the validator — a token signed by issuer A's key but
// claiming issuer B can be accepted because a lookup on `kid` returns
// whichever key was loaded for that issuer. This test exercises the
// behavioural contract end-to-end: two KeySources serve distinct keys under
// the same kid, two validators are bound to the respective issuers, and we
// verify cross-issuer tokens are rejected.

func TestJWKSValidator_CrossIssuerSignatureConfusionRejected(t *testing.T) {
	// Two distinct issuer key pairs, both advertising the same KID.
	keyA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen keyA: %v", err)
	}
	keyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen keyB: %v", err)
	}

	const sharedKID = "shared-kid"
	const issuerA = "https://issuer.example/A"
	const issuerB = "https://issuer.example/B"

	vA := auth.NewValidatorFromSource(staticKeySource{sharedKID: &keyA.PublicKey}, issuerA)
	vB := auth.NewValidatorFromSource(staticKeySource{sharedKID: &keyB.PublicKey}, issuerB)

	now := float64(time.Now().Unix())
	exp := float64(time.Now().Add(time.Hour).Unix())

	// Confusion attempt 1: token signed with keyA but claiming iss=issuerB.
	// Validator B should reject (signature won't match keyB which is what
	// validator B's source serves under sharedKID).
	confusionToken := signTestToken(t, keyA, sharedKID, map[string]any{
		"iss":          issuerB, // claim wrong issuer
		"exp":          exp,
		"iat":          now,
		"caas_user_id": "attacker",
		"caas_org_id":  "org-attack",
	})
	if _, err := vB.Validate(confusionToken); err == nil {
		t.Fatal("validator B accepted a token signed by issuer A's key — cross-issuer confusion not blocked")
	}

	// Confusion attempt 2: token signed with keyA and correctly claiming
	// iss=issuerA, but presented to validator B. Validator B must reject on
	// issuer mismatch even before reaching the signature check; importantly
	// it must not somehow surface keyA's key under sharedKID for itself.
	correctlyClaimedToken := signTestToken(t, keyA, sharedKID, map[string]any{
		"iss":          issuerA,
		"exp":          exp,
		"iat":          now,
		"caas_user_id": "userA",
		"caas_org_id":  "org-A",
	})
	if _, err := vB.Validate(correctlyClaimedToken); err == nil {
		t.Fatal("validator B accepted a token whose iss claim doesn't match its configured issuer")
	}

	// Sanity: legitimate single-issuer paths still work.
	tokenA := signTestToken(t, keyA, sharedKID, map[string]any{
		"iss":          issuerA,
		"exp":          exp,
		"iat":          now,
		"caas_user_id": "userA",
		"caas_org_id":  "org-A",
	})
	if _, err := vA.Validate(tokenA); err != nil {
		t.Fatalf("validator A rejected its own legitimate token: %v", err)
	}

	tokenB := signTestToken(t, keyB, sharedKID, map[string]any{
		"iss":          issuerB,
		"exp":          exp,
		"iat":          now,
		"caas_user_id": "userB",
		"caas_org_id":  "org-B",
	})
	if _, err := vB.Validate(tokenB); err != nil {
		t.Fatalf("validator B rejected its own legitimate token: %v", err)
	}
}
