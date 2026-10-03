package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
)

// KeySource retrieves RSA public keys by KID for JWT signature verification.
// The server's implementation reads the in-process key store
// (NewLocalKeySource).
type KeySource interface {
	GetKey(kid string) (*rsa.PublicKey, error)
}

// ErrKeyNotFound is returned by KeySource implementations when the requested
// KID is not a key that verifies now. Callers match it with errors.Is.
var ErrKeyNotFound = errors.New("kid not found")

// localKeySource returns public keys from the in-process KeyStore.
type localKeySource struct {
	ks KeyStore
}

// NewLocalKeySource returns a KeySource that reads the given in-process
// KeyStore.
func NewLocalKeySource(ks KeyStore) KeySource {
	return &localKeySource{ks: ks}
}

// GetKey returns the public key of kid if the store lets it verify now:
// inside its window, and active or still in the grace period an
// invalidation gave it (an invalidated key pair verifies until the end of
// its grace period; one whose window has ended, or that was deleted, never
// verifies). Every refusal wraps ErrKeyNotFound.
func (s *localKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	pub, err := s.ks.VerificationKey(kid)
	if err != nil {
		// Double %w: callers match ErrKeyNotFound; the store error is diagnostic.
		return nil, fmt.Errorf("%w (kid=%q): %w", ErrKeyNotFound, kid, err)
	}
	return pub, nil
}
