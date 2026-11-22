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
		{"issuer_mismatch vs sig_failure", ErrIssuerMismatch, ErrSignatureFailure},
		{"issuer_mismatch vs claims_failure", ErrIssuerMismatch, ErrClaimsFailure},
		{"sig_failure vs claims_failure", ErrSignatureFailure, ErrClaimsFailure},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) {
			t.Errorf("%s: errors.Is reports same identity", p.name)
		}
	}
}
