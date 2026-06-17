package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
)

// These tests verify the refined error semantics required by ChainedValidator:
//   kid unknown to the source   → ErrUnknownKID    (chain falls through)
//   sig verification failure    → ErrSignatureFailure (hard-fail)
//   iss mismatch (post-verify)  → ErrIssuerMismatch (hard-fail)
//   expired / invalid claims    → ErrClaimsFailure  (hard-fail)
//
// Pre-existing single-validator callers see 401 in all cases; only the typed-
// sentinel distinction changes.

func TestJWKSValidator_UnknownKidReturnsErrUnknownKID(t *testing.T) {
	v := newTestJWKSValidator(t, "cyoda")
	tok := signTokenWithEphemeralKey(t, "unknown-kid", "cyoda", "u1", "org1", 60)
	_, err := v.Validate(tok)
	if !errors.Is(err, ErrUnknownKID) {
		t.Errorf("err = %v, want ErrUnknownKID", err)
	}
}

func TestJWKSValidator_IssMismatchReturnsErrIssuerMismatch(t *testing.T) {
	v, kid, priv := newTestJWKSValidatorWithKey(t, "cyoda")
	tok := signTokenWithKey(t, kid, priv, "https://evil.example", "u1", "org1", 60)
	_, err := v.Validate(tok)
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Errorf("err = %v, want ErrIssuerMismatch", err)
	}
}

func TestJWKSValidator_BadSignatureReturnsErrSignatureFailure(t *testing.T) {
	v, kid, _ := newTestJWKSValidatorWithKey(t, "cyoda")
	wrongPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := signTokenWithKey(t, kid, wrongPriv, "cyoda", "u1", "org1", 60)
	_, err := v.Validate(tok)
	if !errors.Is(err, ErrSignatureFailure) {
		t.Errorf("err = %v, want ErrSignatureFailure", err)
	}
}

func TestJWKSValidator_ExpiredReturnsErrClaimsFailure(t *testing.T) {
	v, kid, priv := newTestJWKSValidatorWithKey(t, "cyoda")
	tok := signTokenWithKey(t, kid, priv, "cyoda", "u1", "org1", -120) // past
	_, err := v.Validate(tok)
	if !errors.Is(err, ErrClaimsFailure) {
		t.Errorf("err = %v, want ErrClaimsFailure", err)
	}
}

func TestJWKSValidator_MissingKidReturnsErrClaimsFailure(t *testing.T) {
	v := newTestJWKSValidator(t, "cyoda")
	tok := signTokenNoKID(t, "cyoda", "u1", "org1", 60)
	_, err := v.Validate(tok)
	if !errors.Is(err, ErrClaimsFailure) {
		t.Errorf("err = %v, want ErrClaimsFailure", err)
	}
}
