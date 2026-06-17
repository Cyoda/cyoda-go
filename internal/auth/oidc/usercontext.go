package oidc

import (
	"fmt"
	"strings"
	"unicode/utf8"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const maxSubLength = 255

// subValidationSentinel is wrapped by buildOIDCUserContext into the OIDCValidator's
// ErrClaimsFailure error chain. Internal to this package — callers use errors.Is
// against auth.ErrClaimsFailure (the OIDCValidator does the outer wrap).
var subValidationSentinel = fmt.Errorf("oidc: sub validation failed")

// buildOIDCUserContext implements D23. Caller is OIDCValidator step 8.
// `claims` is the verified token claims map. `defaultRolesClaim` is the
// global default (CYODA_OIDC_ROLES_CLAIM, typically "roles").
//
// UserID is namespaced as "oidc:<providerUUID>:<sub>" and is OPAQUE downstream —
// no consumer is permitted to parse it back into its components. UserName is
// `sub` for display. Tenant.ID is the provider's OwnerLegalEntityID — claims
// `caas_user_id`, `caas_org_id`, `tid`, `tenant`, `org` from the token are
// explicitly ignored (attacker-controlled when the IdP is external).
//
// sub is validated: must be present, non-empty, ≤255 chars, with no ASCII
// control characters (\x00-\x1f, \x7f). Violations return an error wrapping
// subValidationSentinel with subcode `missing_sub` or `invalid_sub`.
func buildOIDCUserContext(p *OidcProvider, claims map[string]any, defaultRolesClaim string) (*spi.UserContext, error) {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("%w: missing_sub", subValidationSentinel)
	}
	if utf8.RuneCountInString(sub) > maxSubLength {
		return nil, fmt.Errorf("%w: invalid_sub: %d chars > %d", subValidationSentinel, utf8.RuneCountInString(sub), maxSubLength)
	}
	for _, r := range sub {
		if r < 0x20 || r == 0x7f {
			return nil, fmt.Errorf("%w: invalid_sub: contains control character U+%04X", subValidationSentinel, r)
		}
	}

	rolesClaimName := defaultRolesClaim
	if p.RolesClaim != nil && *p.RolesClaim != "" {
		rolesClaimName = *p.RolesClaim
	}
	roles := extractRoles(claims[rolesClaimName])

	return &spi.UserContext{
		UserID:   "oidc:" + p.ID.String() + ":" + sub,
		UserName: sub,
		Tenant: spi.Tenant{
			ID:   spi.TenantID(p.OwnerLegalEntityID.String()),
			Name: p.OwnerLegalEntityID.String(),
		},
		Roles: roles,
	}, nil
}

// extractRoles accepts JSON array-of-strings, []string, or a space-delimited
// string (OAuth2 scope convention per RFC 6749 §3.3 / RFC 8693 §4.2).
// Empty/missing → empty slice.
func extractRoles(claim any) []string {
	switch v := claim.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return strings.Fields(v)
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
