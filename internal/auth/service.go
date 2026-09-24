package auth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
)

// AuthConfig holds configuration for the AuthService.
type AuthConfig struct {
	SigningKeyPEM   string          // PEM-encoded RSA private key
	Issuer          string          // e.g., "cyoda"
	ExpirySeconds   int             // e.g., 3600
	TrustedKeyStore TrustedKeyStore // optional: externally-provided persistent store; if nil, uses in-memory
	IAMFeatures     IAMFeatures     // IAM feature surface for /oauth/keys/* and bootstrap key config
}

// AuthService wires together all auth components and exposes HTTP handlers.
// Public endpoints (token, JWKS) are on Handler().
// Admin endpoints (key mgmt, M2M mgmt, trusted keys) are on AdminHandler()
// and must be wrapped with authentication middleware by the caller.
type AuthService struct {
	keyStore     *InMemoryKeyStore
	trustedStore TrustedKeyStore
	m2mStore     *InMemoryM2MClientStore
	signingKID   string
	issuer       string
	handler      http.Handler
	adminHandler http.Handler
}

// NewAuthService creates a fully wired AuthService from the given config.
func NewAuthService(config AuthConfig) (*AuthService, error) {
	// Apply defaults only for a wholly unset IAMFeatures, so callers that
	// don't set the field (e.g. tests) still get the default bootstrap
	// audience and IAM limits, and a caller that sets any field keeps it.
	if config.IAMFeatures == (IAMFeatures{}) {
		config.IAMFeatures = DefaultIAMFeatures()
	}

	privateKey, err := ParseRSAPrivateKeyFromPEM([]byte(config.SigningKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse signing key: %w", err)
	}

	keyStore := NewInMemoryKeyStore()
	var trustedStore TrustedKeyStore
	if config.TrustedKeyStore != nil {
		trustedStore = config.TrustedKeyStore
	} else {
		trustedStore = NewInMemoryTrustedKeyStore()
	}
	m2mStore := NewInMemoryM2MClientStore()

	// Derive KID deterministically from the public key so all nodes sharing the
	// same RSA key produce the same KID. This is required for multi-node clusters
	// where any node must validate tokens issued by any other node.
	pubDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key for KID: %w", err)
	}
	kidHash := sha256.Sum256(pubDER)
	signingKID := hex.EncodeToString(kidHash[:16])

	// Register the signing key as the initial active key pair. It lives as
	// long as the configuration that supplies it, so it has no window: no
	// ValidTo, and a zero ValidFrom that no clock can be before. A window
	// counted from node start would differ per node, reset on every restart,
	// and — once past — stop that node verifying the cluster's tokens.
	kp := &KeyPair{
		KID:        signingKID,
		Audience:   config.IAMFeatures.BootstrapAudience,
		Algorithm:  "RS256",
		PublicKey:  &privateKey.PublicKey,
		PrivateKey: privateKey,
		Active:     true,
	}
	if err := keyStore.Save(kp, RotateOptions{}); err != nil {
		return nil, fmt.Errorf("save bootstrap key: %w", err)
	}

	// Build handlers.
	jwksHandler := NewJWKSHandler(keyStore)
	tokenHandler := NewTokenHandler(keyStore, trustedStore, m2mStore, config.Issuer, config.ExpirySeconds)

	// Public mux: token issuance and JWKS (no auth required).
	publicMux := http.NewServeMux()
	publicMux.Handle("GET /.well-known/jwks.json", jwksHandler)
	publicMux.Handle("POST /oauth/token", tokenHandler)

	// Admin mux: key management (requires auth + ROLE_ADMIN).
	// Trusted-key endpoints moved to chi adapters in a prior milestone.
	// M2M-client endpoints moved to chi adapters in account/m2m_adapter.go.
	adminMux := http.NewServeMux()

	return &AuthService{
		keyStore:     keyStore,
		trustedStore: trustedStore,
		m2mStore:     m2mStore,
		signingKID:   signingKID,
		issuer:       config.Issuer,
		handler:      publicMux,
		adminHandler: adminMux,
	}, nil
}

// Handler returns the HTTP handler for public auth endpoints (token, JWKS).
func (s *AuthService) Handler() http.Handler {
	return s.handler
}

// AdminHandler returns the HTTP handler for admin auth endpoints
// (key management, M2M client management, trusted keys).
// The caller MUST wrap this with authentication + authorization middleware.
func (s *AuthService) AdminHandler() http.Handler {
	return s.adminHandler
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

// SigningKID returns the key ID used for signing tokens.
func (s *AuthService) SigningKID() string {
	return s.signingKID
}
