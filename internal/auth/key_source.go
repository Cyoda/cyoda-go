package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"time"
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

func (s *localKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	kp, err := s.ks.Get(kid)
	if err != nil {
		// Double %w so callers can errors.Is against both ErrKeyNotFound
		// (semantic) and the underlying KeyStore error (diagnostic).
		return nil, fmt.Errorf("%w (kid=%q): %w", ErrKeyNotFound, kid, err)
	}
	// An invalidated key pair must not validate signatures. Returning the
	// public key for an inactive kid lets tokens signed under that kid keep
	// passing validation after rotation — defeating the point of Invalidate.
	if !kp.Active {
		return nil, fmt.Errorf("%w (kid=%q): key invalidated", ErrKeyNotFound, kid)
	}
	// A key pair verifies only inside its window, the same window GetActive
	// signs within: tokens it signed stop verifying when the window ends.
	now := time.Now()
	if now.Before(kp.ValidFrom) || (kp.ValidTo != nil && !now.Before(*kp.ValidTo)) {
		return nil, fmt.Errorf("%w (kid=%q): key outside its validity window", ErrKeyNotFound, kid)
	}
	return kp.PublicKey, nil
}
