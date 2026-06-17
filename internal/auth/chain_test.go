package auth

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// mockValidator records how many times Validate was called and returns the
// preconfigured result.
type mockValidator struct {
	calls  int
	result *spi.UserContext
	err    error
}

func (m *mockValidator) Validate(_ string) (*spi.UserContext, error) {
	m.calls++
	return m.result, m.err
}

func makeUC(sub string) *spi.UserContext {
	return &spi.UserContext{
		UserID: sub,
		Tenant: spi.Tenant{ID: spi.TenantID("t1")},
	}
}

func TestChainedValidator_FirstSuccessReturned(t *testing.T) {
	uc := makeUC("alice")
	v1 := &mockValidator{result: uc}
	v2 := &mockValidator{}

	chain := NewChainedValidator(v1, v2)
	got, err := chain.Validate("tok")

	if err != nil {
		t.Fatalf("Validate err = %v, want nil", err)
	}
	if got != uc {
		t.Errorf("Validate result = %v, want %v", got, uc)
	}
	if v1.calls != 1 {
		t.Errorf("v1.calls = %d, want 1", v1.calls)
	}
	if v2.calls != 0 {
		t.Errorf("v2.calls = %d, want 0 (should not be called)", v2.calls)
	}
}

func TestChainedValidator_UnknownKIDFallsThrough(t *testing.T) {
	uc := makeUC("bob")
	v1 := &mockValidator{err: ErrUnknownKID}
	v2 := &mockValidator{result: uc}

	chain := NewChainedValidator(v1, v2)
	got, err := chain.Validate("tok")

	if err != nil {
		t.Fatalf("Validate err = %v, want nil", err)
	}
	if got != uc {
		t.Errorf("Validate result = %v, want %v", got, uc)
	}
	if v1.calls != 1 {
		t.Errorf("v1.calls = %d, want 1", v1.calls)
	}
	if v2.calls != 1 {
		t.Errorf("v2.calls = %d, want 1", v2.calls)
	}
}

func TestChainedValidator_HardFailDoesNotFallThrough(t *testing.T) {
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
		t.Run(hf.name, func(t *testing.T) {
			v1 := &mockValidator{err: hf.err}
			v2 := &mockValidator{result: makeUC("carol")}

			chain := NewChainedValidator(v1, v2)
			got, err := chain.Validate("tok")

			if got != nil {
				t.Errorf("Validate result = %v, want nil", got)
			}
			if !errors.Is(err, hf.err) {
				t.Errorf("Validate err = %v, want errors.Is(%v) = true", err, hf.err)
			}
			if v1.calls != 1 {
				t.Errorf("v1.calls = %d, want 1", v1.calls)
			}
			if v2.calls != 0 {
				t.Errorf("v2.calls = %d, want 0 (hard-fail must not fall through)", v2.calls)
			}
		})
	}
}

func TestChainedValidator_AllFailUnknownKID(t *testing.T) {
	v1 := &mockValidator{err: ErrUnknownKID}
	v2 := &mockValidator{err: ErrUnknownKID}

	chain := NewChainedValidator(v1, v2)
	got, err := chain.Validate("tok")

	if got != nil {
		t.Errorf("Validate result = %v, want nil", got)
	}
	if !errors.Is(err, ErrUnknownKID) {
		t.Errorf("Validate err = %v, want ErrUnknownKID", err)
	}
	if v1.calls != 1 {
		t.Errorf("v1.calls = %d, want 1", v1.calls)
	}
	if v2.calls != 1 {
		t.Errorf("v2.calls = %d, want 1", v2.calls)
	}
}

func TestChainedValidator_EmptyChainReturnsUnknownKID(t *testing.T) {
	chain := NewChainedValidator()
	got, err := chain.Validate("tok")

	if got != nil {
		t.Errorf("Validate result = %v, want nil", got)
	}
	if !errors.Is(err, ErrUnknownKID) {
		t.Errorf("Validate err = %v, want ErrUnknownKID", err)
	}
}
