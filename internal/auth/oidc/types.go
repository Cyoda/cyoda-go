// Package oidc implements the per-tenant OIDC provider registry, the chained
// OIDCValidator, and the cluster-broadcast cache eviction layer.
//
// See docs/superpowers/specs/2026-06-16-284-oidc-providers-design.md and
// docs/adr/0002-federated-identity-provider-architecture.md for the design.
package oidc

import (
	"errors"
	"fmt"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/google/uuid"
)

// OidcProvider is the persisted registry entry per spec §3.3. JSON blob value
// at KV key "<tenantID>:<provider-uuid>" in namespace "oidc-providers".
type OidcProvider struct {
	ID                 uuid.UUID  `json:"id"`
	WellKnownConfigURI string     `json:"wellKnownConfigUri"`          // unique per tenant
	Issuers            []string   `json:"issuers,omitempty"`           // optional pin list (max 10); empty = require iss == DiscoveryDoc.Issuer when known
	ExpectedAudiences  []string   `json:"expectedAudiences,omitempty"` // D20; empty = aud unchecked
	RolesClaim         *string    `json:"rolesClaim,omitempty"`        // D23; nil = use DefaultRolesClaim
	InvalidatedAt      *time.Time `json:"invalidatedAt,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"` // load-bearing for D17 iat-binding
	OwnerLegalEntityID uuid.UUID  `json:"ownerLegalEntityId"`
}

// Active reports whether the provider is currently usable for token validation.
// A nil InvalidatedAt means active; any non-nil value means invalidated.
func (p *OidcProvider) Active() bool { return p.InvalidatedAt == nil }

// UriOwnershipHistory is the D25 cross-tenant audit signal. JSON blob value at
// KV key "_history:<sha256(uri)>" in namespace "oidc-providers". The leading
// underscore disambiguates from tenant-UUID-prefixed keys (UUIDs are hex+dashes).
type UriOwnershipHistory struct {
	CurrentOwner *Owner  `json:"currentOwner,omitempty"` // nil after every owner has Deleted
	Past         []Owner `json:"past"`                   // every deleted-or-superseded owner, oldest first
}

// Owner is a single registration record within UriOwnershipHistory.
type Owner struct {
	TenantID     string     `json:"tenantId"`
	ProviderUUID string     `json:"providerUuid"`
	RegisteredAt time.Time  `json:"registeredAt"`
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}

// Domain errors per §3.1. These are not the chain sentinels (those live in
// internal/auth/errors.go and are used by the validator); these are service-
// layer errors surfaced through the HTTP adapter to client wire codes.
var (
	ErrProviderDuplicate = errors.New("oidc: provider with this wellKnownConfigUri already registered for this tenant")
	ErrProviderNotFound  = errors.New("oidc: provider not found")
	ErrProviderInactive  = errors.New("oidc: provider is invalidated")
	ErrSSRFBlocked       = errors.New("oidc: wellKnownConfigUri resolves to a blocked address range")
	ErrDiscoveryFailed   = errors.New("oidc: failed to fetch discovery document")

	// ErrAmbiguousProvider indicates that multiple OIDC providers across distinct
	// tenants are simultaneously iss-eligible and sig-verifying for the same JWT,
	// and cyoda-go cannot determine which tenant's user-context to assign without
	// an unambiguous audience disambiguator. The token is rejected to prevent
	// silent cross-tenant routing. Admins must set distinct ExpectedAudiences on
	// each tenant's provider to allow shared-IdP deployments.
	//
	// Wrapping auth.ErrUnknownKID makes this a chain-fall-through (the next
	// validator is JWKSValidator, which won't match either). The bearer-auth
	// middleware surfaces it as 401.
	ErrAmbiguousProvider = fmt.Errorf("%w: ambiguous provider — multiple tenants iss-eligible for same JWT", auth.ErrUnknownKID)
)
