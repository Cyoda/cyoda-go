package auth

import (
	"fmt"
	"math"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
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

// Validate parses and validates a JWT token string. On success it returns the
// principal and, for a client-credentials token that carries cgen, the
// client-token marker (nil otherwise).
func (v *JWKSValidator) Validate(tokenString string) (*spi.UserContext, *contract.ClientToken, error) {
	parsed, err := Parse(tokenString)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse token: %w", err)
	}

	if err := EnsureAlgRS256(parsed.Header); err != nil {
		return nil, nil, err
	}

	kid, ok := parsed.Header["kid"].(string)
	if !ok || kid == "" {
		return nil, nil, fmt.Errorf("%w: missing kid in token header", ErrClaimsFailure)
	}

	publicKey, err := v.source.GetKey(kid)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve key %q: %w", kid, err)
	}

	if err := Verify(parsed.SigningInput, parsed.Signature, publicKey); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrSignatureFailure, err)
	}

	if err := ValidateClaims(parsed.Claims, 30*time.Second); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrClaimsFailure, err)
	}

	iss, _ := parsed.Claims["iss"].(string)
	if iss != v.issuer {
		return nil, nil, fmt.Errorf("%w: token iss=%q, expected %q", ErrIssuerMismatch, iss, v.issuer)
	}

	audience := func() string {
		v.mu.RLock()
		defer v.mu.RUnlock()
		return v.audience
	}()
	if audience != "" {
		if err := checkAudience(parsed.Claims["aud"], audience); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrClaimsFailure, err)
		}
	}

	uc, ct, err := v.buildUserContext(parsed.Claims)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build user context: %w", err)
	}

	return uc, ct, nil
}

// buildUserContext maps the claims of a verified token to the principal and,
// for a client-credentials token that carries cgen, the client-token marker:
//
//	act + scopes, no user_roles        → user caas_user_id, roles scopes, executor the act.sub client
//	scopes, no act, no user_roles      → service caas_user_id, roles scopes; marker {caas_user_id, cgen} iff cgen
//	user_roles, no act, no scopes      → user caas_user_id, roles user_roles (the operator's token)
//	neither scopes nor user_roles      → user caas_user_id, no roles
//
// Every other combination of act, scopes and user_roles is refused, so each
// accepted token maps to exactly one row. No error carries a claim value: the
// errors reach slog via logAuthFailure's detail field, and claims are
// attacker-chosen.
func (v *JWKSValidator) buildUserContext(claims map[string]any) (*spi.UserContext, *contract.ClientToken, error) {
	// A caas_user_id that is present names the user, so it must be a string
	// and it must pass the check. Only an absent caas_user_id falls back to
	// sub; an empty or malformed one never does, because that would put a
	// different identity in place of the one the token carries.
	var userID string
	if raw, present := claims["caas_user_id"]; present {
		s, ok := raw.(string)
		if !ok {
			return nil, nil, fmt.Errorf("caas_user_id claim rejected: %w: not a string", common.ErrInvalidUserID)
		}
		userID = s
	} else {
		userID, _ = claims["sub"].(string)
		if userID == "" {
			return nil, nil, fmt.Errorf("missing user identity (caas_user_id or sub claim)")
		}
	}
	// The same format check applies to sub; it also rejects a present but
	// empty caas_user_id. The error carries the reason, never the value: it
	// reaches slog via logAuthFailure's detail field, and the claim is
	// attacker-chosen.
	if err := common.ValidateUserID(userID); err != nil {
		return nil, nil, fmt.Errorf("caas_user_id/sub claim rejected: %w", err)
	}

	orgID, _ := claims["caas_org_id"].(string)
	if orgID == "" {
		return nil, nil, fmt.Errorf("missing caas_org_id claim")
	}

	// The tenant door. The caas_org_id claim is the only place a tenant id
	// enters cyoda-go from outside it, and it covers every HTTP and gRPC
	// request — the gRPC interceptor delegates to this same authenticator.
	// Validating here is what lets every downstream consumer treat the tenant
	// as well-formed without rechecking.
	//
	// The error deliberately carries no part of the claim: it reaches slog via
	// logAuthFailure's detail field, and the claim is attacker-chosen.
	if err := common.ValidateTenantID(spi.TenantID(orgID)); err != nil {
		return nil, nil, fmt.Errorf("caas_org_id claim rejected: %w", err)
	}

	// The row is chosen by which claim KEYS are present, never by how many
	// roles a claim carries: an empty scopes array still makes a client.
	actRaw, hasAct := claims["act"]
	scopes, hasScopes := claims["scopes"]
	userRoles, hasUserRoles := claims["user_roles"]
	cgenRaw, hasCgen := claims["cgen"]

	switch {
	case hasScopes && hasUserRoles:
		return nil, nil, fmt.Errorf("token carries both scopes and user_roles")
	case hasAct && !hasScopes:
		return nil, nil, fmt.Errorf("act claim without scopes")
	}

	var gen uint64
	if hasCgen {
		g, err := secretGeneration(cgenRaw)
		if err != nil {
			return nil, nil, err
		}
		gen = g
	}

	uc := &spi.UserContext{
		UserID:   userID,
		UserName: userID,
		Kind:     spi.PrincipalUser,
		Tenant: spi.Tenant{
			ID:   spi.TenantID(orgID),
			Name: orgID,
		},
	}
	var ct *contract.ClientToken

	switch {
	case hasAct:
		// On behalf of a user: the user is the principal, the client in
		// act.sub executes for them.
		clientID, err := actorClientID(actRaw)
		if err != nil {
			return nil, nil, err
		}
		uc.Roles = extractStringSlice(scopes)
		uc.Executor = &spi.Principal{ID: clientID, Kind: spi.PrincipalService}
	case hasScopes:
		// A client acting for itself.
		uc.Kind = spi.PrincipalService
		uc.Roles = extractStringSlice(scopes)
		if hasCgen {
			ct = &contract.ClientToken{ClientID: userID, Gen: gen}
		}
	case hasUserRoles:
		// The operator's offline token.
		uc.Roles = extractStringSlice(userRoles)
	}
	if uc.Roles == nil { // no roles claim, or one that is not an array
		uc.Roles = []string{}
	}

	return uc, ct, nil
}

// actorClientID returns the client id in an act claim, which must be a JSON
// object whose sub is a client id.
func actorClientID(raw any) (string, error) {
	act, ok := raw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("act claim rejected: not an object")
	}
	sub, ok := act["sub"].(string)
	if !ok || !ValidClientID(sub) {
		return "", fmt.Errorf("act claim rejected: sub is not a client id")
	}
	return sub, nil
}

// maxExactGen is the largest integer a JSON number decoded as float64 still
// holds exactly.
const maxExactGen = 1 << 53

// secretGeneration returns the cgen claim as a uint64. JSON numbers decode as
// float64, so only a non-negative integral value no larger than maxExactGen is
// a generation; anything else is refused rather than rounded.
func secretGeneration(raw any) (uint64, error) {
	f, ok := raw.(float64)
	if !ok || f < 0 || f != math.Trunc(f) || f > maxExactGen {
		return 0, fmt.Errorf("cgen claim rejected: not a non-negative integer")
	}
	return uint64(f), nil
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
