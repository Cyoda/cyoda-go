package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// AuthConfig holds configuration for the AuthService.
type AuthConfig struct {
	SigningKeyPEM     string                 // PEM-encoded RSA private key: the bootstrap key
	Issuer            string                 // e.g., "cyoda"
	Audience          string                 // CYODA_JWT_AUDIENCE; set as aud on issued tokens when not empty
	ExpirySeconds     int                    // e.g., 3600
	IAMFeatures       IAMFeatures            // IAM feature surface for /oauth/keys/* and bootstrap key config
	KV                spi.KeyValueStore      // SYSTEM-tenant KV store; required
	Broadcaster       spi.ClusterBroadcaster // nil on a single node
	ReconcileInterval time.Duration          // re-read interval of both key stores; <= 0 uses the default
	TrustedKeyMetrics ReconcileMetrics       // nil: no metrics
	SigningKeyMetrics ReconcileMetrics       // nil: no metrics
}

// AuthService wires together the auth stores and serves the public auth
// endpoints (token, JWKS) on Handler(). The admin endpoints for key pairs,
// trusted keys and M2M clients are served by internal/domain/account over the
// stores this service exposes.
type AuthService struct {
	keyStore     *KVKeyStore
	trustedStore *KVTrustedKeyStore
	m2mStore     *KVM2MClientStore
	issuer       string
	handler      http.Handler
}

// NewAuthService builds and loads both key stores from the KV store; a failed
// load fails. Start runs their re-read loops.
func NewAuthService(ctx context.Context, config AuthConfig) (*AuthService, error) {
	// Apply defaults only for a wholly unset IAMFeatures, so callers that
	// don't set the field (e.g. tests) still get the default bootstrap
	// audience and IAM limits, and a caller that sets any field keeps it.
	if config.IAMFeatures == (IAMFeatures{}) {
		config.IAMFeatures = DefaultIAMFeatures()
	}
	if err := config.IAMFeatures.Validate(); err != nil {
		return nil, fmt.Errorf("invalid IAM features: %w", err)
	}
	if config.KV == nil {
		return nil, errors.New("auth service requires a KV store")
	}
	privateKey, err := ParseRSAPrivateKeyFromPEM([]byte(config.SigningKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse signing key: %w", err)
	}

	trustedOpts := []KVTrustedKeyStoreOption{
		WithMaxTrustedKeys(config.IAMFeatures.TrustedKeyMaxPerTenant),
		WithReconcileInterval(config.ReconcileInterval),
	}
	if config.TrustedKeyMetrics != nil {
		trustedOpts = append(trustedOpts, WithReconcileMetrics(config.TrustedKeyMetrics))
	}
	if config.Broadcaster != nil {
		trustedOpts = append(trustedOpts, WithTrustedKeyBroadcaster(config.Broadcaster))
	}
	trustedStore, err := NewKVTrustedKeyStore(ctx, config.KV, trustedOpts...)
	if err != nil {
		return nil, err
	}

	// The bootstrap key is built from configuration on every node; its KID
	// is derived from the public key, so every node sharing the key has the
	// same KID. Its stored state, if any, comes from the KV store.
	keyStore, err := NewKVKeyStore(ctx, config.KV, KVKeyStoreConfig{
		Bootstrap:         privateKey,
		Broadcaster:       config.Broadcaster,
		ReconcileInterval: config.ReconcileInterval,
		Metrics:           config.SigningKeyMetrics,
	})
	if err != nil {
		return nil, err
	}
	m2mStore := NewKVM2MClientStore(config.KV, config.IAMFeatures.M2MClientMaxPerTenant)

	// Public mux: token issuance and JWKS (no auth required). A stale JWKS
	// answer asks the caller to retry after one re-read interval.
	publicMux := http.NewServeMux()
	publicMux.Handle("GET /.well-known/jwks.json", NewJWKSHandler(keyStore, keyStore.ReconcileInterval()))
	publicMux.Handle("POST /oauth/token", NewTokenHandler(keyStore, trustedStore, m2mStore, config.Issuer, config.Audience, config.ExpirySeconds))

	return &AuthService{
		keyStore:     keyStore,
		trustedStore: trustedStore,
		m2mStore:     m2mStore,
		issuer:       config.Issuer,
		handler:      publicMux,
	}, nil
}

// Start runs both key stores' re-read loops until ctx ends.
func (s *AuthService) Start(ctx context.Context) {
	s.trustedStore.StartReconcileLoop(ctx)
	s.keyStore.Start(ctx)
}

// Handler returns the HTTP handler for public auth endpoints (token, JWKS).
func (s *AuthService) Handler() http.Handler {
	return s.handler
}

// Issuer returns the configured issuer string.
func (s *AuthService) Issuer() string {
	return s.issuer
}

// KeyStore returns the key store.
func (s *AuthService) KeyStore() KeyStore {
	return s.keyStore
}

// TrustedKeyStore returns the trusted key store.
func (s *AuthService) TrustedKeyStore() TrustedKeyStore {
	return s.trustedStore
}

// M2MClientStore returns the M2M client store.
func (s *AuthService) M2MClientStore() M2MClientStore {
	return s.m2mStore
}
