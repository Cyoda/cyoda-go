package auth

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
)

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

func validateRSAPublicExponent(e *big.Int) (int, error) {
	if e.Sign() <= 0 {
		return 0, fmt.Errorf("exponent must be positive")
	}
	if !e.IsInt64() {
		return 0, fmt.Errorf("exponent too large")
	}
	v := e.Int64()
	if v > int64(math.MaxInt) {
		return 0, fmt.Errorf("exponent too large")
	}
	if v&1 == 0 {
		return 0, fmt.Errorf("exponent must be odd")
	}
	return int(v), nil
}
