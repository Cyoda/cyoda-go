package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
)

// These tests pin that JWKSValidator.Validate returns the correct typed
// sentinel for each distinct failure mode.

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
