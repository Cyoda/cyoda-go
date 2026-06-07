package auth

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
)

// trustedKIDPattern is the character whitelist enforced on trusted-key
// identifiers across every lifecycle endpoint (register, delete, invalidate,
// reactivate). Allowed: ASCII alphanumerics plus '-', '_', '.', length 1..128.
// Anything else (control characters, slashes, query syntax, JSON-breaking
// punctuation, unicode confusables) is rejected at the boundary so neither
// the persistence layer nor downstream logs ever see attacker-controlled
// fragments outside this safe set.
var trustedKIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// MatchesTrustedKIDPattern is the exported form for adapter consumption.
func MatchesTrustedKIDPattern(kid string) bool { return trustedKIDPattern.MatchString(kid) }

// ParseRSAPublicKeyFromJWK is the exported form for adapter consumption.
// Returns a generic error on non-RSA kty; callers needing the specific
// UNSUPPORTED_KEY_TYPE response should check kty before calling.
func ParseRSAPublicKeyFromJWK(jwkData json.RawMessage) (*rsa.PublicKey, error) {
	return parseRSAPublicKeyFromJWK(jwkData)
}

func parseRSAPublicKeyFromJWK(jwkData json.RawMessage) (*rsa.PublicKey, error) {
	if len(jwkData) == 0 {
		return nil, fmt.Errorf("empty JWK")
	}
	var jwk struct{ Kty, N, E string }
	if err := json.Unmarshal(jwkData, &jwk); err != nil {
		return nil, fmt.Errorf("parse JWK: %w", err)
	}
	if jwk.Kty != "RSA" {
		return nil, fmt.Errorf("unsupported key type: %s", jwk.Kty)
	}
	if jwk.N == "" || jwk.E == "" {
		return nil, fmt.Errorf("missing n or e")
	}
	nBytes, err := decodeBase64URL(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("invalid n: %w", err)
	}
	eBytes, err := decodeBase64URL(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("invalid e: %w", err)
	}
	e, err := validateRSAPublicExponent(new(big.Int).SetBytes(eBytes))
	if err != nil {
		return nil, err
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

// validateRSAPublicExponent enforces the integrity invariants on an RSA
// public-key exponent: positive, fits in int, and odd. RFC 3447 allows e in
// [3, 2^256-1] and the practical universe of public exponents (3, 17, 65537)
// all fit comfortably; rejecting anything that overflows int avoids the
// silent-truncation hazard at int(big.Int.Int64()) call sites.
func validateRSAPublicExponent(e *big.Int) (int, error) {
	if e.Sign() <= 0 {
		return 0, fmt.Errorf("rsa exponent must be positive")
	}
	if !e.IsInt64() {
		return 0, fmt.Errorf("rsa exponent does not fit in int64")
	}
	v := e.Int64()
	if v > int64(math.MaxInt) {
		return 0, fmt.Errorf("rsa exponent does not fit in int")
	}
	if v&1 == 0 {
		return 0, fmt.Errorf("rsa exponent must be odd")
	}
	return int(v), nil
}
