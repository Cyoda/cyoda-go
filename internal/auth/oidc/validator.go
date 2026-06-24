package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const iatSkew = 30 * time.Second

// OIDCValidator implements auth.Validator using the OIDC provider Registry.
// Wired into the chain after JWKSValidator (chain order is normative — spec §6).
type OIDCValidator struct {
	registry          *Registry
	defaultRolesClaim string
	clock             func() time.Time
}

// Compile-time guard that OIDCValidator satisfies auth.Validator.
var _ auth.Validator = (*OIDCValidator)(nil)

// NewValidator constructs an OIDCValidator. defaultRolesClaim names the JWT
// claim that carries the user's roles; falls back to "roles" when empty.
func NewValidator(registry *Registry, defaultRolesClaim string) *OIDCValidator {
	if defaultRolesClaim == "" {
		defaultRolesClaim = "roles"
	}
	return &OIDCValidator{
		registry:          registry,
		defaultRolesClaim: defaultRolesClaim,
		clock:             time.Now,
	}
}

// Validate implements auth.Validator. Steps follow spec §4.3 (rev. 4).
func (v *OIDCValidator) Validate(tokenString string) (*spi.UserContext, error) {
	// Step 1: structural parse + header peek.
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: malformed token", auth.ErrClaimsFailure)
	}
	hdr, claims, err := decodeJWTSegments(parts[0], parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrClaimsFailure, err)
	}

	// Step 2: alg allow-list.
	alg, _ := hdr["alg"].(string)
	if alg != "RS256" {
		return nil, fmt.Errorf("%w: unsupported_alg %q", auth.ErrClaimsFailure, alg)
	}

	kid, _ := hdr["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("%w: missing kid", auth.ErrClaimsFailure)
	}

	// Step 3: exp check (with skew so tokens expiring "right now" are not
	// hard-rejected during network round-trips).
	now := v.clock()
	exp, hasExp := claimAsInt64(claims, "exp")
	if hasExp && time.Unix(exp, 0).Before(now.Add(-iatSkew)) {
		return nil, fmt.Errorf("%w: expired", auth.ErrClaimsFailure)
	}

	iss, _ := claims["iss"].(string)

	// Extract aud for cross-tenant disambiguation (Layer 1, Critical audit fix #284).
	// For the resolver we use the first audience string found (single string or
	// first element of array). The full audience check at Step 8 uses matchAudience
	// which handles the complete array form — this extract is only for routing.
	audRaw := audPrimaryString(claims["aud"])

	// Step 4: Registry resolution. Propagate sentinels unchanged so the
	// ChainedValidator receives them with their original identity.
	resolution, err := v.registry.ResolveKey(kid, iss, audRaw)
	if err != nil {
		return nil, err
	}

	// Step 5 (D17): iat-binding with 30s skew. Tokens issued before the
	// provider was registered (minus skew) are rejected to prevent spillover
	// across cross-tenant URI re-registrations.
	iat, hasIat := claimAsInt64(claims, "iat")
	if hasIat && time.Unix(iat, 0).Before(resolution.Provider.CreatedAt.Add(-iatSkew)) {
		return nil, fmt.Errorf("%w: iat=%d before provider.CreatedAt=%d - %ds skew",
			auth.ErrTokenPreTransition, iat, resolution.Provider.CreatedAt.Unix(), int(iatSkew.Seconds()))
	}

	// Step 6 (D6): signature verification. On failure evict the kid entry
	// from the registry so the next call goes through the cold path.
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed signature: %w", auth.ErrSignatureFailure, err)
	}
	if err := verifyRS256(signingInput, sig, resolution.PublicKey); err != nil {
		v.registry.EvictKidEntry(kid, resolution.ProviderRef)
		return nil, fmt.Errorf("%w: %w", auth.ErrSignatureFailure, err)
	}

	// Step 7: nbf-style check on iat. If iat is in the future beyond the
	// skew window the token is not yet valid.
	if hasIat && time.Unix(iat, 0).After(now.Add(iatSkew)) {
		return nil, fmt.Errorf("%w: nbf (iat in future)", auth.ErrClaimsFailure)
	}

	// Step 8 (D20): audience check.
	if len(resolution.Provider.ExpectedAudiences) > 0 {
		if !matchAudience(claims["aud"], resolution.Provider.ExpectedAudiences) {
			return nil, fmt.Errorf("%w: audience", auth.ErrClaimsFailure)
		}
	}

	// Step 9 (D23): UserContext extraction.
	uc, err := buildOIDCUserContext(resolution.Provider, claims, v.defaultRolesClaim)
	if err != nil {
		if errors.Is(err, subValidationSentinel) {
			return nil, fmt.Errorf("%w: %w", auth.ErrClaimsFailure, err)
		}
		return nil, err
	}
	return uc, nil
}

// decodeJWTSegments decodes header + payload from their base64url segments.
// The signature segment is left to the caller because verification is performed
// against the raw signing input (header + "." + payload).
func decodeJWTSegments(hdrSeg, claimsSeg string) (map[string]any, map[string]any, error) {
	hdrBytes, err := base64.RawURLEncoding.DecodeString(hdrSeg)
	if err != nil {
		return nil, nil, fmt.Errorf("malformed header segment: %w", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, nil, fmt.Errorf("malformed header json: %w", err)
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(claimsSeg)
	if err != nil {
		return nil, nil, fmt.Errorf("malformed claims segment: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, nil, fmt.Errorf("malformed claims json: %w", err)
	}
	return hdr, claims, nil
}

// claimAsInt64 reads a numeric claim; JSON-decoded numbers are float64.
func claimAsInt64(claims map[string]any, key string) (int64, bool) {
	if v, ok := claims[key].(float64); ok {
		return int64(v), true
	}
	return 0, false
}

// matchAudience accepts aud as either a single string or a JSON array of
// strings per RFC 7519 §4.1.3. Returns true if any element matches an
// entry in expected.
func matchAudience(audClaim any, expected []string) bool {
	asString := func(s string) bool {
		for _, e := range expected {
			if e == s {
				return true
			}
		}
		return false
	}
	switch v := audClaim.(type) {
	case string:
		return asString(v)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && asString(s) {
				return true
			}
		}
	}
	return false
}

// audPrimaryString extracts the routing-disambiguation audience from a parsed
// claims map. If the JWT's `aud` claim is a JSON array (RFC 7519 §4.1.3
// permits both string and array forms), this returns element [0] — the IdP
// determines the array order, so the same JWT can in principle route to
// different providers across IdP versions if the IdP changes its ordering.
//
// LIMITATION: this helper's chosen value is used ONLY for cross-tenant
// resolution disambiguation in disposeCandidates. The full audience-set
// match against provider.ExpectedAudiences happens later (in Validate
// step 8) and considers all array elements. So a token with aud=["A","B"]
// for two providers expecting "A" and "B" respectively routes via [0] but
// must still pass the full-set check against the chosen provider's expected
// audiences. The misroute risk is bounded to the case where the chosen
// tenant's ExpectedAudiences contains the WRONG element of a multi-aud
// token — in that case, the validator still accepts but assigns the wrong
// tenant context.
//
// Operators in multi-tenant deployments should pin a single audience per
// tenant's ExpectedAudiences to eliminate this attack surface, and IdPs
// should issue single-audience tokens where possible.
func audPrimaryString(audClaim any) string {
	switch v := audClaim.(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// verifyRS256 verifies a RSASSA-PKCS1-v1.5 / SHA-256 signature over the JWT
// signing input (header.payload as raw base64url segments concatenated with ".").
func verifyRS256(signingInput string, sig []byte, pub *rsa.PublicKey) error {
	hashed := sha256.Sum256([]byte(signingInput))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig)
}
