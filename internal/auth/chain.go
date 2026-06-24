package auth

import (
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Validator is the contract every JWT-validating component implements.
// Concrete impls: JWKSValidator (first-party tokens), OIDCValidator
// (federated tokens via the OIDC provider registry).
//
// Returned errors must use one of the sentinels in errors.go so the
// ChainedValidator can correctly distinguish fall-through (ErrUnknownKID)
// from hard-fail (all others).
type Validator interface {
	Validate(tokenString string) (*spi.UserContext, error)
}

// ChainedValidator composes multiple Validators in order. Validate consults
// each in sequence; falls through to the next only on ErrUnknownKID. Any
// other error from any validator is returned immediately (hard-fail).
//
// Chain order is semantically meaningful — see app/app.go for the canonical
// construction. The first-party JWKSValidator runs before OIDCValidator so
// that a first-party kid with a foreign iss hard-fails with ErrIssuerMismatch
// without consulting OIDCValidator (spec §3.4, §11 row 36).
type ChainedValidator struct {
	validators []Validator
}

// NewChainedValidator returns a ChainedValidator preserving the order of its
// arguments. Empty chains are permitted (Validate returns ErrUnknownKID).
func NewChainedValidator(validators ...Validator) *ChainedValidator {
	return &ChainedValidator{validators: validators}
}

func (c *ChainedValidator) Validate(tokenString string) (*spi.UserContext, error) {
	var lastErr error = ErrUnknownKID
	for _, v := range c.validators {
		uc, err := v.Validate(tokenString)
		if err == nil {
			return uc, nil
		}
		if !errors.Is(err, ErrUnknownKID) {
			// Hard-fail sentinel — do not consult subsequent validators.
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}
