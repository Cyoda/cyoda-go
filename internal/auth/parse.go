package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// tokenHeaderClaims is the unauthenticated header+claims peek extracted by
// parseTokenHeader. It is intentionally unexported — callers must perform
// signature verification before trusting any of these fields except kid
// and alg (which are needed pre-verify to pick a KeySource).
type tokenHeaderClaims struct {
	kid string
	alg string
	iss string
	aud string
	sub string
	exp int64
	iat int64
}

// parseTokenHeader decodes a JWT's header and claims segments without verifying
// the signature. Used by both JWKSValidator and OIDCValidator to inspect kid/iss
// before deciding whether to consult a KeySource, and to read sub/iat for D17
// and D23 checks AFTER signature verification.
//
// Returns an error if the token is structurally malformed (wrong segment count,
// non-base64url, non-JSON header or claims). Does NOT validate field types or
// value ranges — that is the caller's responsibility post-verification.
func parseTokenHeader(tokenString string) (*tokenHeaderClaims, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token: expected 3 segments, got %d", len(parts))
	}

	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("malformed header segment: %w", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, fmt.Errorf("malformed header json: %w", err)
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed claims segment: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, fmt.Errorf("malformed claims json: %w", err)
	}

	out := &tokenHeaderClaims{}
	out.kid, _ = hdr["kid"].(string)
	out.alg, _ = hdr["alg"].(string)
	out.iss, _ = claims["iss"].(string)
	out.aud, _ = claims["aud"].(string)
	out.sub, _ = claims["sub"].(string)
	if v, ok := claims["exp"].(float64); ok {
		out.exp = int64(v)
	}
	if v, ok := claims["iat"].(float64); ok {
		out.iat = int64(v)
	}
	return out, nil
}
