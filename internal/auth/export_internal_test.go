package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"net/http"
	"time"

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

// RawRecordForTest reads the stored bytes at kid in the signing-keys
// namespace, nil when absent. Test-only: it lives in a _test.go file.
func (s *KVKeyStore) RawRecordForTest(kid string) []byte {
	b, err := s.kv.Get(context.Background(), signingKeysNamespace, kid)
	if err != nil {
		return nil
	}
	return b
}

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

// NewHTTPJWKSSourceWithTransportForTesting returns a KeySource with a
// caller-supplied transport, for tests that trust an httptest.Server's
// self-signed certificate.
func NewHTTPJWKSSourceWithTransportForTesting(jwksURL, issuer string, cacheTTL time.Duration, transport *http.Transport) KeySource {
	return newHTTPJWKSSource(jwksURL, issuer, cacheTTL, transport)
}

// NewHTTPJWKSSourceWithRootCAsForTesting returns a KeySource built via the
// production transport assembly (TLS 1.3 pinned, no InsecureSkipVerify) with
// the given CertPool substituted as RootCAs. Tests use it to verify that the
// production MinVersion is TLS 1.3 end-to-end against an httptest TLS server.
func NewHTTPJWKSSourceWithRootCAsForTesting(jwksURL, issuer string, cacheTTL time.Duration, rootCAs *x509.CertPool) KeySource {
	transport := defaultJWKSTransport()
	transport.TLSClientConfig.RootCAs = rootCAs
	return newHTTPJWKSSource(jwksURL, issuer, cacheTTL, transport)
}
