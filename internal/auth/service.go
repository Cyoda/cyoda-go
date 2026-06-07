package auth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

// AuthConfig holds configuration for the AuthService.
type AuthConfig struct {
	SigningKeyPEM   string          // PEM-encoded RSA private key
	Issuer          string          // e.g., "cyoda"
	ExpirySeconds   int             // e.g., 3600
	TrustedKeyStore TrustedKeyStore // optional: externally-provided persistent store; if nil, uses in-memory
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
	kid := hex.EncodeToString(kidHash[:16])

	// Register the signing key as the initial active key pair.
	kp := &KeyPair{
		KID:        kid,
		Audience:   "client", // bootstrap audience config will land in Task 14
		Algorithm:  "RS256",
		PublicKey:  &privateKey.PublicKey,
		PrivateKey: privateKey,
		Active:     true,
		ValidFrom:  time.Now().UTC(),
	}
	if err := keyStore.Save(kp, RotateOptions{}); err != nil {
		return nil, fmt.Errorf("failed to save signing key: %w", err)
	}

	// Build handlers.
	jwksHandler := NewJWKSHandler(keyStore)
	tokenHandler := NewTokenHandler(keyStore, trustedStore, m2mStore, config.Issuer, config.ExpirySeconds)
	trustedHandler := NewTrustedKeysHandler(trustedStore)
	m2mHandler := NewM2MHandler(m2mStore)

	// Public mux: token issuance and JWKS (no auth required).
	publicMux := http.NewServeMux()
	publicMux.Handle("GET /.well-known/jwks.json", jwksHandler)
	publicMux.Handle("POST /oauth/token", tokenHandler)

	// Admin mux: key management, trusted keys, M2M clients (requires auth + ROLE_ADMIN).
	adminMux := http.NewServeMux()
	adminMux.Handle("/oauth/keys/trusted/", trustedHandler)
	adminMux.Handle("/oauth/keys/trusted", trustedHandler)
	adminMux.Handle("/account/m2m/", m2mHandler)
	adminMux.Handle("/account/m2m", m2mHandler)

	return &AuthService{
		keyStore:     keyStore,
		trustedStore: trustedStore,
		m2mStore:     m2mStore,
		signingKID:   kid,
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
