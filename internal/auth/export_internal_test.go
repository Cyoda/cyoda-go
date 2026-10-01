package auth

import (
	"context"
	"crypto/rsa"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// OpenPrivateKeyForTest opens a sealed key and returns the RSA key behind the
// signer. Test-only: it lives in a _test.go file.
func OpenPrivateKeyForTest(boot *rsa.PrivateKey, owner string, meta KeyMeta, sealed []byte) (*rsa.PrivateKey, error) {
	v, err := NewWrappedVault(boot, owner)
	if err != nil {
		return nil, err
	}
	s, err := v.Open(context.Background(), meta, sealed)
	if err != nil {
		return nil, err
	}
	return s.(rsaSigner).key, nil
}

// ReconcileForTest re-reads the store now, instead of waiting for a change
// message or the periodic loop. Test-only: it lives in a _test.go file.
func (s *KVKeyStore) ReconcileForTest(ctx context.Context) error { return s.rep.Reconcile(ctx) }

// ReconcileForTest re-reads the store now, instead of waiting for a change
// message or the periodic loop. Test-only: it lives in a _test.go file.
func (s *KVTrustedKeyStore) ReconcileForTest(ctx context.Context) error {
	return s.rep.Reconcile(ctx)
}

// busy reports whether the runner is still running a job.
func (c *coalescingRunner) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// TrustedKeyKVKeyForTesting is the KV key of a trusted key, for tests that
// predict KV keys (e.g. for injection mocks).
func TrustedKeyKVKeyForTesting(tenantID spi.TenantID, kid string) string {
	return trustedKeyKey(tenantID, kid)
}
