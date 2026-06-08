package auth_test

import (
	"encoding/base64"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestParseRSAPublicKeyFromJWK_RejectsOversizedModulus(t *testing.T) {
	// 513-byte modulus exceeds the 4096-bit (512-byte) cap.
	big513 := make([]byte, 513)
	for i := range big513 {
		big513[i] = 0xFF
	}
	n := base64.RawURLEncoding.EncodeToString(big513)
	// Use a valid exponent (65537 = 0x010001).
	e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
	jwk := []byte(`{"kty":"RSA","n":"` + n + `","e":"` + e + `"}`)
	_, err := auth.ParseRSAPublicKeyFromJWK(jwk)
	if err == nil {
		t.Fatal("expected error for oversized modulus (>512 bytes / 4096-bit), got nil")
	}
}

func TestParseRSAPublicKeyFromJWK_Accepts4096BitModulus(t *testing.T) {
	// 512-byte modulus is exactly at the 4096-bit cap — must be accepted.
	big512 := make([]byte, 512)
	for i := range big512 {
		big512[i] = 0xFF
	}
	n := base64.RawURLEncoding.EncodeToString(big512)
	e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
	jwk := []byte(`{"kty":"RSA","n":"` + n + `","e":"` + e + `"}`)
	_, err := auth.ParseRSAPublicKeyFromJWK(jwk)
	if err != nil {
		t.Fatalf("4096-bit modulus should be accepted, got: %v", err)
	}
}
