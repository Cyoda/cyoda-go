package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
)

// KeySource retrieves RSA public keys by KID for JWT signature verification.
// Implementations may fetch from a JWKS HTTP endpoint, from a local in-process
// key store, or from any other source. The validator depends only on this
// interface so the transport story can evolve without touching validation logic.
type KeySource interface {
	GetKey(kid string) (*rsa.PublicKey, error)
}

// ErrKeyNotFound is returned by KeySource implementations when the requested
// KID is not known. Callers that need to distinguish "unknown key" from
// "transport failure" can use errors.Is.
var ErrKeyNotFound = errors.New("kid not found")

// localKeySource returns public keys directly from the in-process KeyStore,
// with no HTTP round-trip.
type localKeySource struct {
	ks KeyStore
}

// NewLocalKeySource returns a KeySource that reads directly from the given
// in-process KeyStore. This is the default for the built-in IAM: no JWKS
// HTTP fetch is needed when the signing keys are already in the same process.
func NewLocalKeySource(ks KeyStore) KeySource {
	return &localKeySource{ks: ks}
}

// GetKey returns the public key of kid if the store lets it verify now:
// active and inside its window (an invalidated key pair, or one whose window
// has ended, verifies nothing). Every refusal wraps ErrKeyNotFound.
func (s *localKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	pub, err := s.ks.VerificationKey(kid)
	if err != nil {
		// Double %w: callers match ErrKeyNotFound; the store error is diagnostic.
		return nil, fmt.Errorf("%w (kid=%q): %w", ErrKeyNotFound, kid, err)
	}
	return pub, nil
}
