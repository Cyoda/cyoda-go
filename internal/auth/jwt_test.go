package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	return key
}

func TestSignParseVerifyRoundTrip(t *testing.T) {
	key := generateTestKey(t)
	claims := map[string]any{
		"sub":       "user123",
		"tenant_id": "tenant-abc",
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
		"iat":       float64(time.Now().Unix()),
	}

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), "kid-1")
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	parsed, err := auth.Parse(token)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Verify header
	if alg, _ := parsed.Header["alg"].(string); alg != "RS256" {
		t.Errorf("expected alg RS256, got %s", alg)
	}
	if kid, _ := parsed.Header["kid"].(string); kid != "kid-1" {
		t.Errorf("expected kid kid-1, got %s", kid)
	}

	// Verify claims
	if sub, _ := parsed.Claims["sub"].(string); sub != "user123" {
		t.Errorf("expected sub user123, got %s", sub)
	}

	// Verify signature
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &key.PublicKey); err != nil {
		t.Fatalf("Verify failed: %v", err)
	}

	// Validate claims
	if err := auth.ValidateClaims(parsed.Claims, 0); err != nil {
		t.Fatalf("ValidateClaims failed: %v", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	key := generateTestKey(t)
	claims := map[string]any{
		"sub": "user123",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
		"iat": float64(time.Now().Add(-2 * time.Hour).Unix()),
	}

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), "kid-1")
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	parsed, err := auth.Parse(token)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	err = auth.ValidateClaims(parsed.Claims, 0)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected 'expired' in error, got: %v", err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	key := generateTestKey(t)
	claims := map[string]any{
		"sub": "user123",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}

	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), "kid-1")
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	// Tamper with payload: replace the claims part
	parts := strings.SplitN(token, ".", 3)
	tamperedClaims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin","exp":9999999999}`))
	tamperedToken := parts[0] + "." + tamperedClaims + "." + parts[2]

	parsed, err := auth.Parse(tamperedToken)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	err = auth.Verify(parsed.SigningInput, parsed.Signature, &key.PublicKey)
	if err == nil {
		t.Fatal("expected verification to fail for tampered payload")
	}
}

func TestInvalidSignatureRejected(t *testing.T) {
	key1 := generateTestKey(t)
	key2 := generateTestKey(t)

	claims := map[string]any{
		"sub": "user123",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}

	// Sign with key1
	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key1), "kid-1")
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	parsed, err := auth.Parse(token)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Verify with key2 — should fail
	err = auth.Verify(parsed.SigningInput, parsed.Signature, &key2.PublicKey)
	if err == nil {
		t.Fatal("expected verification to fail with wrong key")
	}
}

func TestMalformedTokenParseFails(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"empty string", ""},
		{"one part", "abc"},
		{"two parts", "abc.def"},
		{"invalid base64 header", "!!!.def.ghi"},
		{"invalid base64 claims", "eyJhbGciOiJSUzI1NiJ9.!!!.ghi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := auth.Parse(tt.token)
			if err == nil {
				t.Errorf("expected error for malformed token %q, got nil", tt.token)
			}
		})
	}
}

func TestParseRSAPrivateKeyFromPEM(t *testing.T) {
	// Generate a key
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	// Encode to PEM (PKCS1 format)
	derBytes := x509.MarshalPKCS1PrivateKey(key)
	var pemBuf strings.Builder
	if err := pem.Encode(&pemBuf, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: derBytes,
	}); err != nil {
		t.Fatalf("failed to encode PEM: %v", err)
	}

	// Parse back
	parsed, err := auth.ParseRSAPrivateKeyFromPEM([]byte(pemBuf.String()))
	if err != nil {
		t.Fatalf("ParseRSAPrivateKeyFromPEM failed: %v", err)
	}

	// Verify the parsed key works for sign/verify
	claims := map[string]any{
		"sub": "test",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(parsed), "kid-pem")
	if err != nil {
		t.Fatalf("Sign with parsed key failed: %v", err)
	}
	pt, err := auth.Parse(token)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := auth.Verify(pt.SigningInput, pt.Signature, &parsed.PublicKey); err != nil {
		t.Fatalf("Verify with parsed key failed: %v", err)
	}

	// Test invalid PEM
	_, err = auth.ParseRSAPrivateKeyFromPEM([]byte("not a pem"))
	if err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
}
