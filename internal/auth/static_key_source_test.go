package auth_test

import (
	"crypto/rsa"
	"fmt"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// staticKeySource is a KeySource over a fixed kid→key map, for exercising
// JWKSValidator without an HTTP JWKS server.
type staticKeySource map[string]*rsa.PublicKey

func (s staticKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q: %w", kid, auth.ErrKeyNotFound)
}
