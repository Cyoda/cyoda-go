package oidc

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
	"strings"
	"sync"
	"testing"
	"time"
)

// FixtureIdP is an httptest-backed OIDC Identity Provider for use in unit and
// integration tests. It serves /.well-known/openid-configuration and /jwks,
// and can sign JWTs using its managed RSA key pairs.
//
// All methods are safe for concurrent use.
type FixtureIdP struct {
	Server  *httptest.Server
	Issuer  string
	JWKSURI string

	mu      sync.Mutex
	keys    map[string]*rsa.PrivateKey // kid → private key
	revoked map[string]bool            // kids excluded from JWKS responses
}

// NewFixtureIdP creates a fresh IdP with a default RSA-2048 key under kid="default".
// The HTTP server is started immediately and t.Cleanup registers its shutdown.
func NewFixtureIdP(t *testing.T) *FixtureIdP {
	t.Helper()

	defaultKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("FixtureIdP: generate default key: %v", err)
	}

	f := &FixtureIdP{
		keys:    map[string]*rsa.PrivateKey{"default": defaultKey},
		revoked: map[string]bool{},
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

// serveDiscovery serves the OpenID Connect discovery document.
func (f *FixtureIdP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]string{
		"issuer":   f.Issuer,
		"jwks_uri": f.JWKSURI,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// fixtureJWKEntry is the JWK wire format for an RSA public key (RFC 7517 + 7518).
type fixtureJWKEntry struct {
	Kty string `json:"kty"`
	KID string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// serveJWKS serves all non-revoked public keys as a JWK Set.
func (f *FixtureIdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var keys []fixtureJWKEntry
	for kid, priv := range f.keys {
		if f.revoked[kid] {
			continue
		}
		keys = append(keys, rsaPublicKeyToJWK(kid, &priv.PublicKey))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// rsaPublicKeyToJWK converts an RSA public key to a JWK entry.
// Modulus N is base64url-encoded as big-endian bytes per RFC 7518 §6.3.1.
// Exponent E is encoded as minimal big-endian bytes (65537 → 3 bytes 0x01 0x00 0x01).
func rsaPublicKeyToJWK(kid string, pub *rsa.PublicKey) fixtureJWKEntry {
	return fixtureJWKEntry{
		Kty: "RSA",
		KID: kid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(bigIntFromInt(pub.E).Bytes()),
	}
}

// bigIntFromInt converts an int to a *big.Int (used for exponent encoding).
func bigIntFromInt(n int) *big.Int {
	return big.NewInt(int64(n))
}

// SignJWT signs the given claims using the private key registered under kid.
// The JWT header sets alg="RS256" and kid. Claims are NOT auto-populated —
// the caller must provide iss, aud, exp, iat, sub as needed.
func (f *FixtureIdP) SignJWT(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()

	f.mu.Lock()
	priv, ok := f.keys[kid]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("FixtureIdP.SignJWT: unknown kid %q", kid)
	}

	hdr := map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"}
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		t.Fatalf("FixtureIdP.SignJWT: marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("FixtureIdP.SignJWT: marshal claims: %v", err)
	}

	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := hdrB64 + "." + claimsB64

	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("FixtureIdP.SignJWT: sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// RotateKey generates a new RSA-2048 key pair with a fresh auto-generated kid.
// Existing keys are NOT removed. Returns the new kid.
func (f *FixtureIdP) RotateKey(t *testing.T) string {
	t.Helper()

	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("FixtureIdP.RotateKey: generate key: %v", err)
	}

	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatalf("FixtureIdP.RotateKey: generate kid random bytes: %v", err)
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
func (f *FixtureIdP) RevokeKey(t *testing.T, kid string) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keys[kid]; !ok {
		t.Fatalf("FixtureIdP.RevokeKey: unknown kid %q", kid)
	}
	f.revoked[kid] = true
}

// AddKeyForKID installs a specific private key under the given kid. Useful
// for planting a key with a well-known kid value (e.g., to collide with a
// first-party kid or to satisfy the validator's key-source lookup).
func (f *FixtureIdP) AddKeyForKID(t *testing.T, kid string, priv *rsa.PrivateKey) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = priv
}

// ---------------------------------------------------------------------------
// Smoke tests
// ---------------------------------------------------------------------------

func TestFixtureIdP_DiscoveryAndJWKS(t *testing.T) {
	f := NewFixtureIdP(t)

	// --- discovery endpoint ---
	resp, err := http.Get(f.Issuer + "/.well-known/openid-configuration") //nolint:noctx
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discovery status = %d, want 200", resp.StatusCode)
	}
	var disc map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if disc["issuer"] != f.Issuer {
		t.Errorf("discovery issuer = %q, want %q", disc["issuer"], f.Issuer)
	}
	if disc["jwks_uri"] != f.JWKSURI {
		t.Errorf("discovery jwks_uri = %q, want %q", disc["jwks_uri"], f.JWKSURI)
	}

	// --- JWKS endpoint ---
	resp2, err := http.Get(f.JWKSURI) //nolint:noctx
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("jwks status = %d, want 200", resp2.StatusCode)
	}
	var jwks struct {
		Keys []fixtureJWKEntry `json:"keys"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(jwks.Keys) == 0 {
		t.Error("JWKS returned no keys")
	}
}

func TestFixtureIdP_SignJWT_VerifiesAgainstPublicKey(t *testing.T) {
	f := NewFixtureIdP(t)

	tok := f.SignJWT(t, "default", map[string]any{
		"sub": "alice",
		"iss": f.Issuer,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})

	// Parse the compact serialization.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("SignJWT returned malformed token: %q", tok)
	}

	// Fetch the JWKS and locate the "default" key.
	pub := fetchPublicKeyFromJWKS(t, f, "default")

	// Verify the RS256 signature.
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	hashed := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig); err != nil {
		t.Errorf("signature verification failed: %v", err)
	}
}

func TestFixtureIdP_RotateAndRevoke(t *testing.T) {
	f := NewFixtureIdP(t)

	newKID := f.RotateKey(t)

	// Both kids must appear in JWKS after rotation.
	kids := fetchJWKSKids(t, f)
	if !kids[newKID] {
		t.Errorf("rotated kid %q not in JWKS after rotation", newKID)
	}
	if !kids["default"] {
		t.Errorf("original default kid not in JWKS after rotation")
	}

	// Revoke the default key — it must disappear from JWKS.
	f.RevokeKey(t, "default")
	kids = fetchJWKSKids(t, f)
	if kids["default"] {
		t.Errorf("revoked default kid still present in JWKS")
	}
	if !kids[newKID] {
		t.Errorf("non-revoked kid %q missing from JWKS after revocation of default", newKID)
	}
}

// ---------------------------------------------------------------------------
// Internal test helpers
// ---------------------------------------------------------------------------

// fetchJWKSKids fetches the JWKS from f and returns the set of present kids.
func fetchJWKSKids(t *testing.T, f *FixtureIdP) map[string]bool {
	t.Helper()
	resp, err := http.Get(f.JWKSURI) //nolint:noctx
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []fixtureJWKEntry `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	out := make(map[string]bool, len(jwks.Keys))
	for _, k := range jwks.Keys {
		out[k.KID] = true
	}
	return out
}

// fetchPublicKeyFromJWKS fetches the JWKS from f and reconstructs the RSA
// public key for the given kid.
func fetchPublicKeyFromJWKS(t *testing.T, f *FixtureIdP, kid string) *rsa.PublicKey {
	t.Helper()
	resp, err := http.Get(f.JWKSURI) //nolint:noctx
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []fixtureJWKEntry `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	for _, k := range jwks.Keys {
		if k.KID != kid {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			t.Fatalf("decode JWK N for kid=%q: %v", kid, err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			t.Fatalf("decode JWK E for kid=%q: %v", kid, err)
		}
		n := new(big.Int).SetBytes(nBytes)
		e := new(big.Int).SetBytes(eBytes)
		return &rsa.PublicKey{N: n, E: int(e.Int64())}
	}
	t.Fatalf("kid=%q not found in JWKS", kid)
	return nil
}
