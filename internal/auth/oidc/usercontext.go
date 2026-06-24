package oidc

import (
	"fmt"
	"strings"
	"unicode/utf8"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
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
	// Defence-in-depth: if the provider somehow got persisted with a nil
	// OwnerLegalEntityID (bypassing the adapter's UUID check), refuse to build
	// a UserContext. The synthetic "nil tenant" (00000000-...) would silently
	// merge users from distinct non-UUID tenants into a shared identity downstream.
	if p.OwnerLegalEntityID == uuid.Nil {
		return nil, fmt.Errorf("%w: nil OwnerLegalEntityID (registration bug — adapter should have rejected this)", subValidationSentinel)
	}

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

// extractRoles accepts the value found at the configured rolesClaim and
// returns the user's roles as a flat slice. Recognised shapes:
//
//   - nil / empty → empty slice (authenticated but unprivileged).
//   - string → space-delimited per RFC 6749 §3.3 / RFC 8693 §4.2; a lone
//     non-empty token like "admin" yields ["admin"].
//   - []string / []any (of strings) → use as-is, dropping empty entries
//     and non-string entries in the []any case.
//   - map[string]any / map[string]string → JSON-object claim; the
//     top-level **keys** are the role values, inner values are ignored.
//     This is the shape Zitadel emits for `urn:zitadel:iam:org:project:roles`
//     with projectRoleAssertion=true (inner values are { orgId: domain }
//     routing metadata, irrelevant for role extraction). Empty-string
//     keys are dropped for parity with array handling.
//
// Any other scalar (number, bool, etc.) → empty slice (no panic). Map
// iteration order is non-deterministic per Go's spec; downstream
// consumers (spi.HasRole) and tests are order-independent, so we don't
// pay a sort cost on the per-request hot path.
//
// Role names containing a comma are dropped silently at every shape.
// UserContext.Roles is comma-joined into the CloudEvent `authclaims`
// attribute (internal/grpc/cloudevent.go); a comma in a role name would
// round-trip ambiguously for any consumer that splits on commas. No
// major IdP (Cognito, Zitadel, Auth0, Keycloak) emits commas in role
// names by convention, so filtering here keeps the wire format intact
// without breaking any real-world IdP.
func extractRoles(claim any) []string {
	switch v := claim.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		fields := strings.Fields(v)
		out := make([]string, 0, len(fields))
		for _, s := range fields {
			if validRoleName(s) {
				out = append(out, s)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if validRoleName(s) {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && validRoleName(s) {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(v))
		for k := range v {
			if validRoleName(k) {
				out = append(out, k)
			}
		}
		return out
	case map[string]string:
		out := make([]string, 0, len(v))
		for k := range v {
			if validRoleName(k) {
				out = append(out, k)
			}
		}
		return out
	default:
		return nil
	}
}

// validRoleName reports whether s is usable as a role name. We drop
// empties (parity across all extraction shapes) and anything containing
// a comma — see extractRoles' doc comment for the CloudEvent
// `authclaims` round-trip rationale.
func validRoleName(s string) bool {
	return s != "" && !strings.ContainsRune(s, ',')
}
