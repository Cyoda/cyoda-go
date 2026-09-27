package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestRSASigner_SignsPKCS1v15SHA256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewRSASigner(key)
	if pub, ok := s.Public().(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Fatalf("Public() = %v, want the key's public key", s.Public())
	}
	digest := sha256.Sum256([]byte("payload"))
	sig, err := s.Sign(context.Background(), digest[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestSign_UsesSigner(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Sign(context.Background(), map[string]any{"sub": "x"}, auth.NewRSASigner(key), "kid-1")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parsed, err := auth.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Header["kid"] != "kid-1" {
		t.Fatalf("kid = %v", parsed.Header["kid"])
	}
	h := sha256.Sum256([]byte(parsed.SigningInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], parsed.Signature); err != nil {
		t.Fatalf("token signature does not verify: %v", err)
	}
}
