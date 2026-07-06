package e2e_test

import (
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// newMockIAMServer builds an in-process server using mock IAM mode with the
// trusted-key feature gate enabled. In this configuration (F-a):
//   - OIDC adapter is nil  → 7 OIDC ops return 501
//   - keyStore is nil      → 5 keypair ops return 501
//   - trustedKeyStore nil, feature gate ON → 5 trusted ops return 501
//   - m2mClientStore nil   → 4 M2M ops return 501
//
// RequireAdmin is satisfied automatically: the mock auth service injects the
// default user context (ROLE_ADMIN, ROLE_M2M) on every request, so no bearer
// token is needed.
func newMockIAMServer(t *testing.T) (string, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.StorageBackend = "memory"
	cfg.Cluster.Enabled = false
	cfg.IAM.Mode = "mock"
	cfg.IAM.TrustedKeyRegistrationEnabled = true // gate ON → nil store → 501

	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv.URL, func() {
		srv.Close()
		a.Shutdown()
		_ = a.Close()
	}
}

// newFeatureOffServer builds an in-process server using mock IAM mode with the
// trusted-key feature gate disabled (F-b). In this configuration the 5 trusted
// ops hit the feature gate before the nil-store gate and return
// 404 FEATURE_DISABLED. The other 16 IAM-gated ops still return 501.
// No bearer token needed (mock auth injects ROLE_ADMIN).
func newFeatureOffServer(t *testing.T) (string, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.StorageBackend = "memory"
	cfg.Cluster.Enabled = false
	cfg.IAM.Mode = "mock"
	// cfg.IAM.TrustedKeyRegistrationEnabled is false in DefaultConfig — leave it.

	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv.URL, func() {
		srv.Close()
		a.Shutdown()
		_ = a.Close()
	}
}
