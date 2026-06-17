package parity

// ParityFixtureIdP is an in-process mock OIDC Identity Provider for use in
// parity tests. It serves /.well-known/openid-configuration and /jwks, and
// can sign JWTs using its managed RSA key pairs.
//
// This is a minimal reimplementation of internal/auth/oidc.FixtureIdP (which
// lives in a _test.go file and therefore cannot be imported from outside the
// internal/auth/oidc package). Changes to the original MUST be reflected here.
//
// All methods are safe for concurrent use.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ParityFixtureIdP is an httptest-backed OIDC Identity Provider. It binds on
// a random localhost port so parity tests do not need external network access.
// The CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true subprocess env var allows the
// server-side SSRF check to pass for 127.0.0.1 addresses.
type ParityFixtureIdP struct {
	Server     *httptest.Server
	Issuer     string
	JWKSURI    string
	DefaultKid string // the auto-generated kid used for the initial key

	mu            sync.Mutex
	keys          map[string]*rsa.PrivateKey // kid → private key
	revoked       map[string]bool            // kids excluded from JWKS responses
	discoveryHits int                        // count of /.well-known/openid-configuration requests
}

// parityJWKEntry is the JWK wire format for an RSA public key (RFC 7517 + 7518).
type parityJWKEntry struct {
	Kty string `json:"kty"`
	KID string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// NewParityFixtureIdP creates a fresh mock IdP with a single RSA-2048 key.
// The initial kid is auto-generated (not "default") so that parallel or
// sequential parity tests do not collide in the shared registry kidIndex.
// The kid is available via f.DefaultKid. The HTTP server is started
// immediately and t.Cleanup registers its shutdown.
func NewParityFixtureIdP(t *testing.T) *ParityFixtureIdP {
	t.Helper()

	// Generate a unique kid for this IdP instance to avoid kidIndex
	// collisions across subtests that share the same in-process server.
	kidBytes := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, kidBytes); err != nil {
		t.Fatalf("NewParityFixtureIdP: generate kid bytes: %v", err)
	}
	initialKid := fmt.Sprintf("key-%x", kidBytes)

	defaultKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("NewParityFixtureIdP: generate default key: %v", err)
	}

	f := &ParityFixtureIdP{
		DefaultKid: initialKid,
		keys:       map[string]*rsa.PrivateKey{initialKid: defaultKey},
		revoked:    map[string]bool{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.serveDiscovery)
	mux.HandleFunc("/jwks", f.serveJWKS)

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)

	f.Issuer = f.Server.URL
	f.JWKSURI = f.Server.URL + "/jwks"

	return f
}

func (f *ParityFixtureIdP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.discoveryHits++
	f.mu.Unlock()

	doc := map[string]string{
		"issuer":   f.Issuer,
		"jwks_uri": f.JWKSURI,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *ParityFixtureIdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var keys []parityJWKEntry
	for kid, priv := range f.keys {
		if f.revoked[kid] {
			continue
		}
		keys = append(keys, parityRSAPublicKeyToJWK(kid, &priv.PublicKey))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

func parityRSAPublicKeyToJWK(kid string, pub *rsa.PublicKey) parityJWKEntry {
	return parityJWKEntry{
		Kty: "RSA",
		KID: kid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(new(big.Int).SetInt64(int64(pub.E)).Bytes()),
	}
}

// SignJWT signs the given claims using the private key registered under kid.
// Claims are NOT auto-populated — the caller must supply iss, aud, exp, iat,
// sub, scopes, caas_org_id, caas_user_id as needed for cyoda-go's validator.
func (f *ParityFixtureIdP) SignJWT(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()

	f.mu.Lock()
	priv, ok := f.keys[kid]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("ParityFixtureIdP.SignJWT: unknown kid %q", kid)
	}

	hdr := map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"}
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		t.Fatalf("ParityFixtureIdP.SignJWT: marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("ParityFixtureIdP.SignJWT: marshal claims: %v", err)
	}

	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := hdrB64 + "." + claimsB64

	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("ParityFixtureIdP.SignJWT: sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// MintTenantJWT signs a minimal JWT with the cyoda-go claims shape required
// for the OIDC validator to accept it. tenantID is used as the caas_org_id
// claim. The iss claim is set to f.Issuer so the registry can map the token
// to the registered provider.
func (f *ParityFixtureIdP) MintTenantJWT(t *testing.T, kid, tenantID string) string {
	t.Helper()
	now := time.Now()
	return f.SignJWT(t, kid, map[string]any{
		"sub":          "oidc-test-" + tenantID[:8],
		"iss":          f.Issuer,
		"caas_user_id": "oidc-test-" + tenantID[:8],
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_ADMIN"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
	})
}

// MintTenantJWTWithIssuer is like MintTenantJWT but allows overriding the iss
// claim — used by cross-signing tests (row 27) that need a JWT with a foreign
// iss.
func (f *ParityFixtureIdP) MintTenantJWTWithIssuer(t *testing.T, kid, tenantID, issuer string) string {
	t.Helper()
	now := time.Now()
	return f.SignJWT(t, kid, map[string]any{
		"sub":          "oidc-cross-" + tenantID[:8],
		"iss":          issuer,
		"caas_user_id": "oidc-cross-" + tenantID[:8],
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_ADMIN"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
	})
}

// RotateKey generates a new RSA-2048 key pair with a fresh auto-generated kid.
// Existing keys are NOT removed. Returns the new kid.
func (f *ParityFixtureIdP) RotateKey(t *testing.T) string {
	t.Helper()

	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ParityFixtureIdP.RotateKey: generate key: %v", err)
	}

	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatalf("ParityFixtureIdP.RotateKey: generate kid random bytes: %v", err)
	}
	kid := fmt.Sprintf("key-%x", b)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = newKey
	return kid
}

// RevokeKey marks kid as revoked. Subsequent JWKS responses will omit it.
// The private key is NOT destroyed — SignJWT with a revoked kid still works
// (revocation is a JWKS-publishing concern, not a key-destruction concern).
func (f *ParityFixtureIdP) RevokeKey(t *testing.T, kid string) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keys[kid]; !ok {
		t.Fatalf("ParityFixtureIdP.RevokeKey: unknown kid %q", kid)
	}
	f.revoked[kid] = true
}

// WellKnownURI returns the discovery endpoint URL for this mock IdP.
func (f *ParityFixtureIdP) WellKnownURI() string {
	return f.Server.URL + "/.well-known/openid-configuration"
}

// DiscoveryHitCount returns the number of times the discovery endpoint has
// been requested since this IdP was created. Used by chain-order tests that
// want to assert that a validator was (or was not) reached during token
// validation — JWKS endpoint is fetched after discovery, so zero discovery
// hits implies the OIDCValidator cold path was not entered.
func (f *ParityFixtureIdP) DiscoveryHitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoveryHits
}

// MintJWTWithIat signs a JWT with an explicit iat timestamp. All other claims
// match MintTenantJWT. Used by D17 iat-binding tests that need precise control
// over when the token is considered to have been issued.
func (f *ParityFixtureIdP) MintJWTWithIat(t *testing.T, kid, tenantID string, iat time.Time) string {
	t.Helper()
	now := time.Now()
	return f.SignJWT(t, kid, map[string]any{
		"sub":          "oidc-iat-" + tenantID[:8],
		"iss":          f.Issuer,
		"caas_user_id": "oidc-iat-" + tenantID[:8],
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_ADMIN"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          iat.Unix(),
	})
}

// SetJWKSForKid adds (or replaces) a JWK entry for a specific kid, using a
// new RSA key pair that shares the kid string with an EXISTING key in this IdP
// OR uses the provided private key. This is used by D6/D11 scenarios that need
// two IdPs publishing the same kid to simulate a cross-tenant kid collision.
//
// AddSharedKidEntry creates a fresh RSA key, stores it under the given kid
// (evicting any previous mapping), and returns the new kid. The JWKS endpoint
// will serve this new public key for the given kid.
func (f *ParityFixtureIdP) AddSharedKidEntry(t *testing.T, kid string) {
	t.Helper()
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ParityFixtureIdP.AddSharedKidEntry: generate key: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = newKey
}
