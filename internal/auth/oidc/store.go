package oidc

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// OidcProviderStore is the persistence interface for OIDC providers and their
// cross-tenant URI ownership history. The single implementation (KVOidcProviderStore)
// sits on top of spi.KeyValueStore and uses one namespace "oidc-providers" with
// composite keys per spec §3.3:
//
//	<tenantID>:<provider-uuid>       → JSON(OidcProvider)
//	<tenantID>:uri:<sha256(uri)>     → <provider-uuid>      (per-tenant unique index)
//	_history:<sha256(uri)>           → JSON(UriOwnershipHistory)  (D25 cross-tenant audit)
//
// All Store operations run under the system tenant context — see app/app.go
// for the bootstrap. Tenancy is enforced via the composite key prefix, not via
// the underlying KV's tenant-at-acquisition scoping.
type OidcProviderStore interface {
	// Register persists a new provider. Caller is responsible for the D11
	// race-validation re-read after Put — the store's contract is "best-effort
	// sequential": Put index, Put blob, return.
	Register(ctx context.Context, p *OidcProvider) error

	// Get retrieves a provider by (tenant, id) with stale-index defence:
	// if the blob's OwnerLegalEntityID doesn't match the requesting tenant,
	// the store treats this as orphaned and returns ErrProviderNotFound.
	Get(ctx context.Context, tenantID spi.TenantID, providerID string) (*OidcProvider, error)

	// GetByURI retrieves a provider by (tenant, wellKnownConfigUri). Same
	// stale-index defence as Get.
	GetByURI(ctx context.Context, tenantID spi.TenantID, uri string) (*OidcProvider, error)

	// Update overwrites the blob. Caller is responsible for re-reading first
	// (the adapter does the stale-index defence + 404 check).
	Update(ctx context.Context, p *OidcProvider) error

	// Delete removes the blob AND the per-tenant URI index entry. Does NOT
	// touch the cross-tenant ownership history — Service.Delete handles that
	// separately via PutURIHistory.
	Delete(ctx context.Context, tenantID spi.TenantID, providerID, uri string) error

	// ListByTenant returns all providers for one tenant, optionally filtered
	// to active=true.
	ListByTenant(ctx context.Context, tenantID spi.TenantID, activeOnly bool) ([]*OidcProvider, error)

	// LoadAll enumerates every provider across all tenants — used by the
	// startup hook and by Registry.ReloadAll. Returns one item per provider
	// blob (not per index entry; the implementation filters out keys with
	// the ":uri:" or "_history:" substrings).
	LoadAll(ctx context.Context) ([]*OidcProvider, error)

	// GetURIHistory returns the cross-tenant ownership history for one URI,
	// or (nil, nil) if no history exists yet (first registration).
	GetURIHistory(ctx context.Context, uriHash string) (*UriOwnershipHistory, error)

	// PutURIHistory overwrites the ownership-history entry for one URI.
	// Failure is logged ERROR by the caller but does not block the lifecycle
	// operation (D25 is an audit signal, not a correctness gate).
	PutURIHistory(ctx context.Context, uriHash string, h *UriOwnershipHistory) error

	// RaceValidateIndex re-reads the per-tenant URI index after Put to detect
	// the D11 register race. If the stored providerID does not equal expectedID,
	// the caller lost the race; returns the winning providerID and ok=false.
	RaceValidateIndex(ctx context.Context, tenantID spi.TenantID, uri string, expectedID string) (winningID string, ok bool, err error)
}
