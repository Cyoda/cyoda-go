package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestProviderKeySource_DelegatesGetKey(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	delegate := newStaticKeySource(map[string]*rsa.PublicKey{"kid-1": &priv.PublicKey})

	pks := newProviderKeySource(delegate, func() bool { return true })
	got, err := pks.GetKey("kid-1")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if got == nil {
		t.Fatal("got nil key")
	}
}

func TestProviderKeySource_GatesByLifecycle(t *testing.T) {
	delegate := newStaticKeySource(map[string]*rsa.PublicKey{}) // empty — gate must fire, not the delegate
	pks := newProviderKeySource(delegate, func() bool { return false })
	_, err := pks.GetKey("kid-1")
	if err == nil {
		t.Fatal("expected error from invalidated provider, got nil")
	}
	if !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("invalidated-provider error must wrap auth.ErrKeyNotFound; got %v", err)
	}
}

func TestProviderKeySource_PropagatesDelegateErrors(t *testing.T) {
	delegate := newStaticKeySource(map[string]*rsa.PublicKey{})
	pks := newProviderKeySource(delegate, func() bool { return true })

	_, err := pks.GetKey("unknown")
	if err == nil {
		t.Fatal("expected error for unknown kid, got nil")
	}
}

// staticKeySource is a minimal in-package KeySource for these unit tests.
// Implements auth.KeySource via the GetKey(kid) (*rsa.PublicKey, error) contract.
type staticKeySource struct {
	keys map[string]*rsa.PublicKey
}

func newStaticKeySource(keys map[string]*rsa.PublicKey) *staticKeySource {
	return &staticKeySource{keys: keys}
}

func (s *staticKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	if k, ok := s.keys[kid]; ok {
		return k, nil
	}
	return nil, auth.ErrKeyNotFound
}
