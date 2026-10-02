package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// baseAudClaims produces a minimal, otherwise-valid claims map. Tests
// override the "aud" claim to exercise audience handling.
func baseAudClaims(issuer string) map[string]any {
	return map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}
}

// TestJWKSValidator_AcceptsMatchingAudienceString confirms that a token
// whose aud claim is a string equal to the configured expected audience
// passes validation.
func TestJWKSValidator_AcceptsMatchingAudienceString(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	expectedAud := "cyoda-svc"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	v.SetExpectedAudience(expectedAud)

	claims := baseAudClaims(issuer)
	claims["aud"] = expectedAud

	token := signTestToken(t, key, kid, claims)
	if _, _, err := v.Validate(token); err != nil {
		t.Fatalf("validator rejected token with matching aud: %v", err)
	}
}

// TestJWKSValidator_AcceptsMatchingAudienceArray confirms that a token
// whose aud claim is a JSON array containing the expected audience passes.
// RFC 7519 allows aud to be either a string or an array of strings.
func TestJWKSValidator_AcceptsMatchingAudienceArray(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	expectedAud := "cyoda-svc"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	v.SetExpectedAudience(expectedAud)

	claims := baseAudClaims(issuer)
	claims["aud"] = []any{"other-service", expectedAud, "yet-another"}

	token := signTestToken(t, key, kid, claims)
	if _, _, err := v.Validate(token); err != nil {
		t.Fatalf("validator rejected token with aud array containing expected: %v", err)
	}
}

// TestJWKSValidator_RejectsWrongAudience blocks a token minted for a
// different relying party.
func TestJWKSValidator_RejectsWrongAudience(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	v.SetExpectedAudience("cyoda-svc")

	claims := baseAudClaims(issuer)
	claims["aud"] = "other-svc"

	token := signTestToken(t, key, kid, claims)
	_, _, err := v.Validate(token)
	if err == nil {
		t.Fatal("validator accepted token with wrong aud")
	}
	if !strings.Contains(err.Error(), "aud") {
		t.Errorf("expected error mentioning aud, got %v", err)
	}
}

// TestJWKSValidator_RejectsMissingAudienceWhenConfigured blocks a token
// that has no aud claim at all, when the validator has been configured
// with an expected audience.
func TestJWKSValidator_RejectsMissingAudienceWhenConfigured(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	v.SetExpectedAudience("cyoda-svc")

	claims := baseAudClaims(issuer)
	// No "aud" claim.

	token := signTestToken(t, key, kid, claims)
	_, _, err := v.Validate(token)
	if err == nil {
		t.Fatal("validator accepted token with missing aud")
	}
	if !strings.Contains(err.Error(), "aud") {
		t.Errorf("expected error mentioning aud, got %v", err)
	}
}

// TestJWKSValidator_NoAudienceCheckWhenUnconfigured preserves
// backwards-compatible behaviour: a validator with no expected audience
// configured does not reject tokens based on aud (current production
// behaviour until the rollout lands).
func TestJWKSValidator_NoAudienceCheckWhenUnconfigured(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	// No SetExpectedAudience call.

	claims := baseAudClaims(issuer)
	claims["aud"] = "anything-goes"

	token := signTestToken(t, key, kid, claims)
	if _, _, err := v.Validate(token); err != nil {
		t.Fatalf("validator rejected token when no aud was configured: %v", err)
	}
}
