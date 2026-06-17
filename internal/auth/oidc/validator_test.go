package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/google/uuid"
)

// validClaims returns a base set of claims that pass all checks when used with
// the provider returned by newValidatorFixture.
func validClaims(iss string, now time.Time, providerCreatedAt time.Time) map[string]any {
	return map[string]any{
		"sub": "alice",
		"iss": iss,
		"aud": "my-service",
		"iat": float64(now.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
}

// newValidatorFixture sets up a Registry + OidcProvider + RSA key and returns
// the validator, the provider, and the private key for signing test tokens.
func newValidatorFixture(t *testing.T) (v *OIDCValidator, p *OidcProvider, priv *rsa.PrivateKey) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	reg := newTestRegistry(t)
	p = &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://idp.example",
		Issuers:            []string{"https://idp.example"},
		ExpectedAudiences:  []string{"my-service"},
		CreatedAt:          time.Now().Add(-10 * time.Minute),
		OwnerLegalEntityID: uuid.New(),
	}
	reg.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})

	v = NewValidator(reg, "roles")
	return v, p, priv
}

func TestOIDCValidator_HappyPath(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	claims := validClaims(p.Issuers[0], now, p.CreatedAt)
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	uc, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if uc == nil {
		t.Fatal("uc is nil")
	}
	wantUserID := "oidc:" + p.ID.String() + ":alice"
	if uc.UserID != wantUserID {
		t.Errorf("UserID = %q, want %q", uc.UserID, wantUserID)
	}
	wantTenant := p.OwnerLegalEntityID.String()
	if string(uc.Tenant.ID) != wantTenant {
		t.Errorf("Tenant.ID = %q, want %q", uc.Tenant.ID, wantTenant)
	}
}

func TestOIDCValidator_IatBeforeCreatedAtRejected(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	// iat is 5 minutes before providerCreatedAt — well outside 30s skew.
	iat := p.CreatedAt.Add(-5 * time.Minute)
	claims := map[string]any{
		"sub": "alice",
		"iss": p.Issuers[0],
		"aud": "my-service",
		"iat": float64(iat.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrTokenPreTransition) {
		t.Errorf("err = %v, want ErrTokenPreTransition", err)
	}
}

func TestOIDCValidator_IatWithinSkewAccepted(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	// iat is exactly 29s before providerCreatedAt — within the 30s skew.
	iat := p.CreatedAt.Add(-29 * time.Second)
	claims := map[string]any{
		"sub": "alice",
		"iss": p.Issuers[0],
		"aud": "my-service",
		"iat": float64(iat.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if err != nil {
		t.Errorf("Validate: %v (iat within skew should be accepted)", err)
	}
}

func TestOIDCValidator_SigFailureEvictsKid(t *testing.T) {
	// D6: on signature failure, EvictKidEntry must be called so the next
	// resolution attempt goes through the cold path again.
	v, p, _ := newValidatorFixture(t)

	// Generate a DIFFERENT key to sign — signature will fail.
	wrongKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	claims := validClaims(p.Issuers[0], now, p.CreatedAt)
	token := signRS256ForOIDCTest(t, "k1", wrongKey, claims)

	// First call populates kidIndex then fails sig check.
	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrSignatureFailure) {
		t.Errorf("err = %v, want ErrSignatureFailure", err)
	}

	// D6: kidIndex must have been evicted.
	if v.registry.kidIndexContains("k1", p.OwnerLegalEntityID.String(), p.WellKnownConfigURI) {
		t.Error("D6 violated: kidIndex entry still present after ErrSignatureFailure")
	}
}

func TestOIDCValidator_AudienceMismatch(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	claims := map[string]any{
		"sub": "alice",
		"iss": p.Issuers[0],
		"aud": "wrong-service",
		"iat": float64(now.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrClaimsFailure) {
		t.Errorf("err = %v, want ErrClaimsFailure", err)
	}
	if err != nil && err.Error() != "" {
		// subcode check
		assertContains(t, err.Error(), "audience")
	}
}

func TestOIDCValidator_AudienceMatchAcceptsArray(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	// aud as array containing the expected value.
	claims := map[string]any{
		"sub": "alice",
		"iss": p.Issuers[0],
		"aud": []any{"other-service", "my-service"},
		"iat": float64(now.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if err != nil {
		t.Errorf("Validate with aud array: %v", err)
	}
}

func TestOIDCValidator_EmptyExpectedAudiencesAcceptsAny(t *testing.T) {
	// When provider.ExpectedAudiences is empty, aud claim is unchecked (D20).
	reg := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://idp.example",
		Issuers:            []string{"https://idp.example"},
		ExpectedAudiences:  nil, // empty
		CreatedAt:          time.Now().Add(-10 * time.Minute),
		OwnerLegalEntityID: uuid.New(),
	}
	reg.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})
	v := NewValidator(reg, "roles")

	now := time.Now()
	claims := map[string]any{
		"sub": "alice",
		"iss": p.Issuers[0],
		"aud": "anything-goes",
		"iat": float64(now.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if err != nil {
		t.Errorf("Validate with empty ExpectedAudiences: %v", err)
	}
}

func TestOIDCValidator_ExpiredTokenRejected(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	claims := map[string]any{
		"sub": "alice",
		"iss": p.Issuers[0],
		"aud": "my-service",
		"iat": float64(now.Add(-2 * time.Hour).Unix()),
		"exp": float64(now.Add(-1 * time.Hour).Unix()), // expired 1 hour ago
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrClaimsFailure) {
		t.Errorf("err = %v, want ErrClaimsFailure", err)
	}
	assertContains(t, err.Error(), "expired")
}

func TestOIDCValidator_UnsupportedAlgRejected(t *testing.T) {
	v, _, _ := newValidatorFixture(t)

	// Hand-craft a token with alg=HS256 in the header.
	hdr, _ := json.Marshal(map[string]any{"kid": "k1", "alg": "HS256", "typ": "JWT"})
	cl, _ := json.Marshal(map[string]any{"sub": "alice"})
	enc := base64.RawURLEncoding.EncodeToString
	token := enc(hdr) + "." + enc(cl) + ".fakesig"

	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrClaimsFailure) {
		t.Errorf("err = %v, want ErrClaimsFailure", err)
	}
	assertContains(t, err.Error(), "unsupported_alg")
}

func TestOIDCValidator_MissingSubRejected(t *testing.T) {
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	// No sub claim.
	claims := map[string]any{
		"iss": p.Issuers[0],
		"aud": "my-service",
		"iat": float64(now.Unix()),
		"exp": float64(now.Add(5 * time.Minute).Unix()),
	}
	token := signRS256ForOIDCTest(t, "k1", priv, claims)

	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrClaimsFailure) {
		t.Errorf("err = %v, want ErrClaimsFailure", err)
	}
	assertContains(t, err.Error(), "missing_sub")
}

func TestOIDCValidator_PropagatesUnknownKID(t *testing.T) {
	// ErrUnknownKID must propagate as-is so the ChainedValidator falls through.
	v, p, priv := newValidatorFixture(t)
	now := time.Now()
	claims := validClaims(p.Issuers[0], now, p.CreatedAt)
	// Sign with a kid that doesn't exist in the registry.
	token := signRS256ForOIDCTest(t, "no-such-kid", priv, claims)

	_, err := v.Validate(token)
	if !errors.Is(err, auth.ErrUnknownKID) {
		t.Errorf("err = %v, want ErrUnknownKID (chain fall-through)", err)
	}
}

// assertContains fails the test if s does not contain sub.
func assertContains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("expected %q to contain %q", s, sub)
	}
}

func TestOIDCValidator_PropagatesIssuerMismatch(t *testing.T) {
	// Provider registered with Issuers=[expected]; token signed with a key whose
	// kid IS in the registry, but with iss=foreign. ResolveKey returns ErrIssuerMismatch.
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://idp.example",
		Issuers:            []string{"https://idp.example"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: uuid.New(),
	}
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

	v := NewValidator(r, "roles")
	tok := signRS256ForOIDCTest(t, "k1", priv, map[string]any{
		"iss": "https://evil.example", // mismatched
		"sub": "alice",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	_, err := v.Validate(tok)
	if !errors.Is(err, auth.ErrIssuerMismatch) {
		t.Errorf("err = %v, want ErrIssuerMismatch", err)
	}
}

// failingKeySource simulates a JWKS backend that returns a non-ErrKeyNotFound error.
type failingKeySource struct{}

func (f *failingKeySource) GetKey(_ string) (*rsa.PublicKey, error) {
	return nil, errors.New("simulated JWKS network failure")
}

func TestOIDCValidator_PropagatesJWKSUnavailable(t *testing.T) {
	r := newTestRegistry(t)
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://idp.example",
		Issuers:            []string{"https://idp.example"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: uuid.New(),
	}
	r.installForTest(p, &failingKeySource{},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

	v := NewValidator(r, "roles")
	// Token signature/content irrelevant; ResolveKey will return ErrJWKSUnavailable first.
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := signRS256ForOIDCTest(t, "k1", priv, map[string]any{
		"iss": "https://idp.example",
		"sub": "alice",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	_, err := v.Validate(tok)
	if !errors.Is(err, auth.ErrJWKSUnavailable) {
		t.Errorf("err = %v, want ErrJWKSUnavailable", err)
	}
}

func TestOIDCValidator_MalformedTokenRejected(t *testing.T) {
	r := newTestRegistry(t)
	v := NewValidator(r, "roles")
	cases := []struct {
		name, tok string
	}{
		{"empty", ""},
		{"one-segment", "abc"},
		{"two-segments", "abc.def"},
		{"five-segments", "a.b.c.d.e"},
		{"invalid-base64-header", "%%%.%%%.%%%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Validate(c.tok)
			if !errors.Is(err, auth.ErrClaimsFailure) {
				t.Errorf("malformed %q: err = %v, want ErrClaimsFailure", c.name, err)
			}
		})
	}
}

// signRS256ForOIDCTest builds a signed RS256 JWT with the given kid, key, and claims.
func signRS256ForOIDCTest(t *testing.T, kid string, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	hdr, err := json.Marshal(map[string]any{"kid": kid, "alg": "RS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	cl, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	signingInput := enc(hdr) + "." + enc(cl)
	sig, err := signRS256Raw(signingInput, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + enc(sig)
}

// signRS256Raw is the test-side mirror of verifyRS256.
func signRS256Raw(signingInput string, priv *rsa.PrivateKey) ([]byte, error) {
	hashed := sha256.Sum256([]byte(signingInput))
	return rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
}
