package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func encodeSeg(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// nowOffset returns a Unix timestamp offset by sec seconds from now.
func nowOffset(sec int) int64 {
	return time.Now().Add(time.Duration(sec) * time.Second).Unix()
}

// staticKeySource is a test KeySource that resolves a fixed set of kids to public keys.
type staticKeySource map[string]*rsa.PublicKey

func (s staticKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	pub, ok := s[kid]
	if !ok {
		return nil, fmt.Errorf("%w: kid %q not in source", ErrKeyNotFound, kid)
	}
	return pub, nil
}

// newTestJWKSValidator returns a JWKSValidator with an empty key source (no
// registered kids). Useful for tests that expect a key-resolution failure or
// ErrClaimsFailure (missing kid) without needing a valid signing key.
func newTestJWKSValidator(t *testing.T, issuer string) *JWKSValidator {
	t.Helper()
	return NewValidatorFromSource(staticKeySource{}, issuer)
}

// newTestJWKSValidatorWithKey generates a fresh RSA key pair, registers it in
// the validator's key source, and returns the validator together with the kid
// and private key so callers can sign tokens against it.
func newTestJWKSValidatorWithKey(t *testing.T, issuer string) (*JWKSValidator, string, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("newTestJWKSValidatorWithKey: generate key: %v", err)
	}
	kid := "test-kid"
	src := staticKeySource{kid: &priv.PublicKey}
	v := NewValidatorFromSource(src, issuer)
	return v, kid, priv
}

// signTokenWithKey signs a JWT with the given kid and private key.
// expOffsetSec controls the exp claim offset from now (negative = already expired).
func signTokenWithKey(t *testing.T, kid string, priv *rsa.PrivateKey, iss, sub, orgID string, expOffsetSec int) string {
	t.Helper()
	now := time.Now().Unix()
	claims := map[string]any{
		"iss":          iss,
		"sub":          sub,
		"caas_user_id": sub,
		"caas_org_id":  orgID,
		"iat":          float64(now),
		"exp":          float64(nowOffset(expOffsetSec)),
	}
	tok, err := Sign(context.Background(), claims, NewRSASigner(priv), kid)
	if err != nil {
		t.Fatalf("signTokenWithKey: %v", err)
	}
	return tok
}

// signTokenNoKID produces a valid RS256 JWT whose header omits the kid field.
// The token body and signature are valid; only the header kid is absent.
func signTokenNoKID(t *testing.T, iss, sub, orgID string, expOffsetSec int) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("signTokenNoKID: generate key: %v", err)
	}
	now := time.Now().Unix()
	claims := map[string]any{
		"iss":          iss,
		"sub":          sub,
		"caas_user_id": sub,
		"caas_org_id":  orgID,
		"iat":          float64(now),
		"exp":          float64(nowOffset(expOffsetSec)),
	}
	// Build a header without a kid field, then sign manually.
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("signTokenNoKID: marshal header: %v", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("signTokenNoKID: marshal claims: %v", err)
	}
	signingInput := encodeSeg(hb) + "." + encodeSeg(cb)
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("signTokenNoKID: sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// newTestKeyStore is the signing-key store the package's internal tests use:
// a KVKeyStore over a fresh in-memory KV store, with boot as its bootstrap
// key.
func newTestKeyStore(t *testing.T, boot *rsa.PrivateKey) *KVKeyStore {
	t.Helper()
	s, err := NewKVKeyStore(replicaSystemCtx(), newReplicaKV(t), KVKeyStoreConfig{Bootstrap: boot})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newTestAuthService builds an AuthService over a fresh in-memory KV store;
// cfg.KV is filled in.
func newTestAuthService(t *testing.T, cfg AuthConfig) *AuthService {
	t.Helper()
	cfg.KV = newReplicaKV(t)
	svc, err := NewAuthService(replicaSystemCtx(), cfg)
	if err != nil {
		t.Fatalf("NewAuthService failed: %v", err)
	}
	return svc
}
