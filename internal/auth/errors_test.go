package auth

import (
	"errors"
	"testing"
)

func TestSentinelErrorsAreDistinct(t *testing.T) {
	pairs := []struct {
		name string
		a, b error
	}{
		{"unknown_kid vs issuer_mismatch", ErrUnknownKID, ErrIssuerMismatch},
		{"unknown_kid vs sig_failure", ErrUnknownKID, ErrSignatureFailure},
		{"unknown_kid vs claims_failure", ErrUnknownKID, ErrClaimsFailure},
		{"issuer_mismatch vs sig_failure", ErrIssuerMismatch, ErrSignatureFailure},
		{"pre_transition vs unknown_kid", ErrTokenPreTransition, ErrUnknownKID},
		{"jwks_unavailable vs sig_failure", ErrJWKSUnavailable, ErrSignatureFailure},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) {
			t.Errorf("%s: errors.Is reports same identity", p.name)
		}
	}
}

func TestUnknownKIDIsTheOnlyFallthroughSentinel(t *testing.T) {
	// Document the chain contract: ChainedValidator falls through ONLY on
	// ErrUnknownKID. Every other sentinel is a hard-fail. This loop is the
	// executable form of that contract — adding a sentinel to errors.go
	// without considering its chain semantics will not affect this test,
	// but accidentally aliasing one to ErrUnknownKID will.
	hardFails := []struct {
		name string
		err  error
	}{
		{"ErrIssuerMismatch", ErrIssuerMismatch},
		{"ErrSignatureFailure", ErrSignatureFailure},
		{"ErrClaimsFailure", ErrClaimsFailure},
		{"ErrTokenPreTransition", ErrTokenPreTransition},
		{"ErrJWKSUnavailable", ErrJWKSUnavailable},
	}
	for _, hf := range hardFails {
		if errors.Is(hf.err, ErrUnknownKID) {
			t.Errorf("%s reports as ErrUnknownKID — chain would incorrectly fall through on hard-fail", hf.name)
		}
	}
}
