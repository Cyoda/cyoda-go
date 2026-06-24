package oidc

import (
	"crypto/rsa"
	"fmt"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// providerKeySource wraps an underlying auth.KeySource (typically HTTPJWKSSource
// fetching from a discovered jwks_uri) and gates GetKey by a lifecycle
// predicate. When the provider is invalidated, GetKey refuses regardless of
// what the underlying source has cached.
type providerKeySource struct {
	inner    auth.KeySource
	isActive func() bool
}

func newProviderKeySource(inner auth.KeySource, isActive func() bool) *providerKeySource {
	return &providerKeySource{inner: inner, isActive: isActive}
}

func (p *providerKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	if !p.isActive() {
		return nil, fmt.Errorf("%w: provider is invalidated", auth.ErrKeyNotFound)
	}
	return p.inner.GetKey(kid)
}
