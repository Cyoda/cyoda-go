package auth

import (
	"fmt"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// JWKSValidator validates JWT tokens against a KeySource. The transport —
// in-process lookup, HTTPS JWKS fetch, or any future alternative — is pluggable
// behind KeySource; the validator itself only owns issuer, audience, and claims
// validation.
type JWKSValidator struct {
	source   KeySource
	issuer   string
	mu       sync.RWMutex
	audience string
}

// SetExpectedAudience configures the audience value that tokens must
// carry in their aud claim. An empty string disables the check (matches
// pre-hardening behaviour). When set, tokens with a non-matching or
// missing aud are rejected. The check accepts aud as either a string
// or a JSON array of strings (RFC 7519 §4.1.3).
//
// This is a setter rather than a constructor argument so existing
// callers without an audience configured continue to build, and so
// production wiring can opt-in via CYODA_JWT_AUDIENCE at startup.
func (v *JWKSValidator) SetExpectedAudience(aud string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.audience = aud
}

// NewValidatorFromSource returns a JWKSValidator that resolves keys via the
// given KeySource. This is the preferred constructor — callers decide where
// keys come from (in-process, external JWKS, etc).
func NewValidatorFromSource(src KeySource, issuer string) *JWKSValidator {
	return &JWKSValidator{source: src, issuer: issuer}
}

// NewJWKSValidator creates a validator backed by an HTTPS JWKS endpoint with
// TLS 1.3 pinned, 10s request timeout, and the given cache TTL. Preserved as
// a convenience for tests and for future external-IdP wiring; in-process
// callers should use NewValidatorFromSource with NewLocalKeySource.
func NewJWKSValidator(jwksURL, issuer string, cacheTTL time.Duration) *JWKSValidator {
	return NewValidatorFromSource(NewHTTPJWKSSource(jwksURL, issuer, cacheTTL), issuer)
}

// Validate parses and validates a JWT token string, returning a UserContext on success.
func (v *JWKSValidator) Validate(tokenString string) (*spi.UserContext, error) {
	parsed, err := Parse(tokenString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	if err := EnsureAlgRS256(parsed.Header); err != nil {
		return nil, err
	}

	kid, ok := parsed.Header["kid"].(string)
	if !ok || kid == "" {
		return nil, fmt.Errorf("%w: missing kid in token header", ErrClaimsFailure)
	}

	publicKey, err := v.source.GetKey(kid)
	if err != nil {
		return nil, fmt.Errorf("%w: kid %q: %w", ErrUnknownKID, kid, err)
	}

	if err := Verify(parsed.SigningInput, parsed.Signature, publicKey); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureFailure, err)
	}

	if err := ValidateClaims(parsed.Claims, 30*time.Second); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrClaimsFailure, err)
	}

	iss, _ := parsed.Claims["iss"].(string)
	if iss != v.issuer {
		return nil, fmt.Errorf("%w: token iss=%q, expected %q", ErrIssuerMismatch, iss, v.issuer)
	}

	v.mu.RLock()
	audience := v.audience
	v.mu.RUnlock()
	if audience != "" {
		if err := checkAudience(parsed.Claims["aud"], audience); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrClaimsFailure, err)
		}
	}

	uc, err := v.buildUserContext(parsed.Claims)
	if err != nil {
		return nil, fmt.Errorf("failed to build user context: %w", err)
	}

	return uc, nil
}

// buildUserContext extracts user information from JWT claims.
func (v *JWKSValidator) buildUserContext(claims map[string]any) (*spi.UserContext, error) {
	userID, _ := claims["caas_user_id"].(string)
	if userID == "" {
		userID, _ = claims["sub"].(string)
	}
	if userID == "" {
		return nil, fmt.Errorf("missing user identity (caas_user_id or sub claim)")
	}

	orgID, _ := claims["caas_org_id"].(string)
	if orgID == "" {
		return nil, fmt.Errorf("missing caas_org_id claim")
	}

	// Kind branches on claim-KEY presence, not on how many roles it carries:
	// an OBO token's user_roles key marks a human principal even when the
	// array is empty (an unprivileged user is still a user). Only a token
	// that carries scopes but no user_roles key at all is a service
	// (client_credentials) principal. Absent both, default to user — the
	// attribution-safe choice when no signal says otherwise.
	kind := spi.PrincipalUser
	if _, hasUserRoles := claims["user_roles"]; !hasUserRoles {
		if _, hasScopes := claims["scopes"]; hasScopes {
			kind = spi.PrincipalService
		}
	}

	// OBO tokens carry user_roles, client_credentials tokens carry scopes.
	// Try user_roles first (OBO), fall back to scopes (client_credentials).
	roles := extractStringSlice(claims["user_roles"])
	if len(roles) == 0 {
		roles = extractStringSlice(claims["scopes"])
	}

	return &spi.UserContext{
		UserID:   userID,
		UserName: userID,
		Kind:     kind,
		Tenant: spi.Tenant{
			ID:   spi.TenantID(orgID),
			Name: orgID,
		},
		Roles: roles,
	}, nil
}

// checkAudience verifies that the token's aud claim contains the expected
// audience. RFC 7519 §4.1.3 permits aud to be a single string or an array
// of strings; both forms are accepted here.
func checkAudience(claim any, expected string) error {
	if claim == nil {
		return fmt.Errorf("missing aud claim (required: %q)", expected)
	}
	switch v := claim.(type) {
	case string:
		if v == expected {
			return nil
		}
		return fmt.Errorf("aud mismatch: token carries %q, want %q", v, expected)
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == expected {
				return nil
			}
		}
		return fmt.Errorf("aud array does not include required audience %q", expected)
	case []string:
		for _, s := range v {
			if s == expected {
				return nil
			}
		}
		return fmt.Errorf("aud array does not include required audience %q", expected)
	default:
		return fmt.Errorf("aud claim has unsupported type %T", claim)
	}
}

// extractStringSlice converts a claim value to []string, handling both []interface{} and []string.
func extractStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []string:
		result := make([]string, len(val))
		copy(result, val)
		return result
	case []any:
		result := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	default:
		return nil
	}
}
