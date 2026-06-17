package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
)

// Service orchestrates the 7 lifecycle operations per spec §5. Mutates
// store, mutates registry, broadcasts (fire-and-forget per D7). D25
// ownership-transition audit log is emitted inline — no separate audit.go.
type Service struct {
	store    OidcProviderStore
	registry *Registry
	logger   *slog.Logger
	clock    func() time.Time
}

// NewService constructs a Service. logger may be nil (slog.Default() is used).
func NewService(store OidcProviderStore, registry *Registry, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, registry: registry, logger: logger, clock: time.Now}
}

// RegisterInput is the validated decode of POST /oauth/oidc/providers.
type RegisterInput struct {
	TenantID           spi.TenantID
	WellKnownConfigURI string
	Issuers            []string
	ExpectedAudiences  []string
	RolesClaim         *string
	OwnerLegalEntityID uuid.UUID
}

// Register implements §5.1.
//
// Steps:
//  1. Per-tenant duplicate check via GetByURI.
//  2. Build OidcProvider with new UUID + CreatedAt.
//  3. Persist via store.Register.
//  4. D11 race-validation re-read: if this caller lost the race, delete own
//     blob and return ErrProviderDuplicate.
//  5. D25 ownership-history update + audit log.
//  6. registry.reloadOne (sync; failures non-fatal — discovery logs internally).
//  7. broadcastOp "reload" fire-and-forget per D7.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*OidcProvider, error) {
	// Step 1: per-tenant duplicate check.
	if existing, _ := s.store.GetByURI(ctx, in.TenantID, in.WellKnownConfigURI); existing != nil {
		return nil, ErrProviderDuplicate
	}

	// Step 2: build the new provider.
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: in.WellKnownConfigURI,
		Issuers:            in.Issuers,
		ExpectedAudiences:  in.ExpectedAudiences,
		RolesClaim:         in.RolesClaim,
		CreatedAt:          s.clock().UTC(),
		OwnerLegalEntityID: in.OwnerLegalEntityID,
	}

	// Step 3: persist.
	if err := s.store.Register(ctx, p); err != nil {
		return nil, fmt.Errorf("oidc: register: %w", err)
	}

	// Step 4: D11 race-validation re-read.
	_, won, err := s.store.RaceValidateIndex(ctx, in.TenantID, in.WellKnownConfigURI, p.ID.String())
	if err != nil {
		_ = s.store.Delete(ctx, in.TenantID, p.ID.String(), in.WellKnownConfigURI)
		return nil, fmt.Errorf("oidc: race-validate: %w", err)
	}
	if !won {
		_ = s.store.Delete(ctx, in.TenantID, p.ID.String(), in.WellKnownConfigURI)
		return nil, ErrProviderDuplicate
	}

	// Step 5: D25 ownership-history update + audit log.
	s.emitOwnershipTransitionAndUpdateHistory(ctx, p, in.TenantID)

	// Step 6: registry warm-up (sync; failures are non-fatal).
	// addToProviderMap ensures the new provider is in r.providers BEFORE
	// reloadOne checks for it (reloadOne's I9 guard silently skips providers
	// that are absent from the in-memory map). This is the local-node
	// equivalent of what ReloadAll does globally.
	s.registry.addToProviderMap(p)
	s.registry.reloadOne(ctx, in.TenantID, in.WellKnownConfigURI)

	// Step 7: broadcast fire-and-forget per D7.
	s.registry.broadcastOp("reload", string(in.TenantID), in.WellKnownConfigURI)

	return p, nil
}

// UpdateInput captures the PATCH body. Boolean flags denote "field was present
// in the PATCH body"; the adapter (Task 6.1) translates tri-state JSON into
// this representation.
type UpdateInput struct {
	TenantID          spi.TenantID
	ProviderID        string
	UpdateIssuers     bool     // field was present in PATCH body
	Issuers           []string // nil with UpdateIssuers=true → clear
	UpdateAudiences   bool
	ExpectedAudiences []string
	UpdateRolesClaim  bool
	RolesClaim        *string // nil with UpdateRolesClaim=true → revert to default
}

// Update implements §5.2. Returns ErrProviderNotFound if the provider does not
// exist, ErrProviderInactive if it has been invalidated.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*OidcProvider, error) {
	p, err := s.store.Get(ctx, in.TenantID, in.ProviderID)
	if err != nil {
		if errors.Is(err, ErrProviderNotFound) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("oidc: update: %w", err)
	}
	if !p.Active() {
		return nil, ErrProviderInactive
	}
	if in.UpdateIssuers {
		p.Issuers = in.Issuers
	}
	if in.UpdateAudiences {
		p.ExpectedAudiences = in.ExpectedAudiences
	}
	if in.UpdateRolesClaim {
		p.RolesClaim = in.RolesClaim
	}
	if err := s.store.Update(ctx, p); err != nil {
		return nil, fmt.Errorf("oidc: store update: %w", err)
	}
	// addToProviderMap refreshes the in-memory provider struct (e.g. updated
	// Issuers) so that disposeCandidates uses the current Issuers list for iss
	// matching. Re-fetch discovery + JWKS to pick up any jwks_uri changes.
	s.registry.addToProviderMap(p)
	s.registry.reloadOne(ctx, in.TenantID, p.WellKnownConfigURI)
	s.registry.broadcastOp("reload", string(in.TenantID), p.WellKnownConfigURI)
	return p, nil
}

// Invalidate implements §5.3. Idempotent: already-invalidated providers return
// nil with a WARN log and no broadcast.
func (s *Service) Invalidate(ctx context.Context, tenant spi.TenantID, providerID string) error {
	p, err := s.store.Get(ctx, tenant, providerID)
	if err != nil {
		if errors.Is(err, ErrProviderNotFound) {
			return ErrProviderNotFound
		}
		return fmt.Errorf("oidc: invalidate: %w", err)
	}
	if !p.Active() {
		// §5.3: already invalidated → idempotent 200, no broadcast.
		s.logger.Warn("oidc.invalidate.already_inactive",
			"pkg", "oidc",
			"provider_uuid", p.ID.String(),
			"tenant", string(tenant))
		return nil
	}
	now := s.clock().UTC()
	p.InvalidatedAt = &now
	if err := s.store.Update(ctx, p); err != nil {
		return fmt.Errorf("oidc: invalidate store: %w", err)
	}
	s.registry.invalidateOne(tenant, p.WellKnownConfigURI)
	s.registry.broadcastOp("invalidate", string(tenant), p.WellKnownConfigURI)
	return nil
}

// ReactivateInput holds the reactivation request parameters.
type ReactivateInput struct {
	TenantID       spi.TenantID
	ProviderID     string
	ReactivateKeys bool // defaults to true at the adapter layer
}

// Reactivate implements §5.4 with D19 conditional sync. Idempotent: already-
// active providers return their current DTO with no broadcast.
func (s *Service) Reactivate(ctx context.Context, in ReactivateInput) (*OidcProvider, error) {
	p, err := s.store.Get(ctx, in.TenantID, in.ProviderID)
	if err != nil {
		if errors.Is(err, ErrProviderNotFound) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("oidc: reactivate: %w", err)
	}
	if p.Active() {
		// §5.4: already active → idempotent 200 with current DTO, no broadcast.
		s.logger.Info("oidc.reactivate.already_active",
			"pkg", "oidc",
			"provider_uuid", p.ID.String(),
			"tenant", string(in.TenantID))
		return p, nil
	}
	p.InvalidatedAt = nil
	if err := s.store.Update(ctx, p); err != nil {
		return nil, fmt.Errorf("oidc: reactivate store: %w", err)
	}
	// D19: try reloadOne which fetches discovery + JWKS. Failure is WARN-logged
	// inside reloadOne; InvalidatedAt is cleared regardless.
	// addToProviderMap re-installs the reactivated provider into r.providers
	// (invalidateOne removed it when the provider was invalidated) so that
	// reloadOne's I9 guard does not silently skip it.
	s.registry.addToProviderMap(p)
	s.registry.reloadOne(ctx, in.TenantID, p.WellKnownConfigURI)
	s.registry.broadcastOp("reload", string(in.TenantID), p.WellKnownConfigURI)
	return p, nil
}

// Delete implements §5.5. Updates D25 ownership history after removing the blob.
func (s *Service) Delete(ctx context.Context, tenant spi.TenantID, providerID string) error {
	p, err := s.store.Get(ctx, tenant, providerID)
	if err != nil {
		if errors.Is(err, ErrProviderNotFound) {
			return ErrProviderNotFound
		}
		return fmt.Errorf("oidc: delete: %w", err)
	}
	if err := s.store.Delete(ctx, tenant, p.ID.String(), p.WellKnownConfigURI); err != nil {
		return fmt.Errorf("oidc: delete store: %w", err)
	}
	// §5.5: D25 ownership-history update on Delete.
	s.markOwnershipDeletedInHistory(ctx, p.WellKnownConfigURI, tenant, p.ID.String())
	s.registry.invalidateOne(tenant, p.WellKnownConfigURI)
	s.registry.broadcastOp("invalidate", string(tenant), p.WellKnownConfigURI)
	return nil
}

// ReloadAll implements §5.6. Rebuilds the in-memory registry from KV and
// broadcasts reload_all.
func (s *Service) ReloadAll(ctx context.Context) error {
	if err := s.registry.ReloadAll(ctx); err != nil {
		return fmt.Errorf("oidc: reload-all: %w", err)
	}
	s.registry.broadcastOp("reload_all", "", "")
	return nil
}

// ListByTenant implements §5.7.
func (s *Service) ListByTenant(ctx context.Context, tenant spi.TenantID, activeOnly bool) ([]*OidcProvider, error) {
	return s.store.ListByTenant(ctx, tenant, activeOnly)
}

// emitOwnershipTransitionAndUpdateHistory implements the D25 Register-time audit.
// Inlined per spec — no separate audit.go.
func (s *Service) emitOwnershipTransitionAndUpdateHistory(ctx context.Context, p *OidcProvider, tenant spi.TenantID) {
	uriHash := sha256Hex(p.WellKnownConfigURI)
	history, err := s.store.GetURIHistory(ctx, uriHash)
	if err != nil {
		s.logger.Error("oidc: get URI history",
			"pkg", "oidc", "uri_hash", uriHash, "error", err.Error())
		return
	}

	// Collect prior/concurrent tenants that are not the current registrant.
	var priors []string
	if history != nil {
		if history.CurrentOwner != nil && history.CurrentOwner.TenantID != string(tenant) {
			priors = append(priors, history.CurrentOwner.TenantID)
		}
		for _, past := range history.Past {
			if past.TenantID != string(tenant) && !containsStr(priors, past.TenantID) {
				priors = append(priors, past.TenantID)
			}
		}
	}
	if len(priors) > 0 {
		s.logger.Info("oidc.cross_tenant_uri_registration",
			"pkg", "oidc",
			"registering_tenant", string(tenant),
			"prior_or_concurrent_tenants", priors,
			"wellknown_uri_hash", uriHash,
			"new_provider_uuid", p.ID.String(),
		)

		// Layer 3 — Cross-tenant audience-overlap WARN (#284 Critical audit fix).
		// If the registering provider and any prior/concurrent provider share an
		// empty or overlapping ExpectedAudiences set, tokens will be rejected as
		// ErrAmbiguousProvider at validation time. Emit a WARN so operators can
		// fix the misconfiguration before it causes user-facing 401s.
		s.warnIfAudienceOverlap(ctx, p, history, uriHash)
	}

	if history == nil {
		history = &UriOwnershipHistory{}
	}
	newOwner := Owner{
		TenantID:     string(tenant),
		ProviderUUID: p.ID.String(),
		RegisteredAt: p.CreatedAt,
	}
	if history.CurrentOwner == nil {
		history.CurrentOwner = &newOwner
	} else {
		// Concurrent ownership case: move prior CurrentOwner to Past (no DeletedAt —
		// it remains active concurrently), install new owner as CurrentOwner.
		history.Past = append(history.Past, *history.CurrentOwner)
		history.CurrentOwner = &newOwner
	}
	if err := s.store.PutURIHistory(ctx, uriHash, history); err != nil {
		s.logger.Error("oidc: put URI history",
			"pkg", "oidc", "uri_hash", uriHash, "error", err.Error())
	}
}

// markOwnershipDeletedInHistory updates D25 history on Delete: marks the
// CurrentOwner with DeletedAt and moves it to Past. Nop if no history exists
// or the CurrentOwner does not match the deleting tenant+provider.
func (s *Service) markOwnershipDeletedInHistory(ctx context.Context, uri string, tenant spi.TenantID, providerID string) {
	uriHash := sha256Hex(uri)
	history, err := s.store.GetURIHistory(ctx, uriHash)
	if err != nil || history == nil {
		// No history → nothing to mark. Do not surface as an error.
		return
	}
	if history.CurrentOwner == nil ||
		history.CurrentOwner.TenantID != string(tenant) ||
		history.CurrentOwner.ProviderUUID != providerID {
		// Current owner is someone else (concurrent owner case) — leave it.
		return
	}
	now := s.clock().UTC()
	deleted := *history.CurrentOwner
	deleted.DeletedAt = &now
	history.Past = append(history.Past, deleted)
	history.CurrentOwner = nil
	if err := s.store.PutURIHistory(ctx, uriHash, history); err != nil {
		s.logger.Error("oidc: put URI history on delete",
			"pkg", "oidc", "uri_hash", uriHash, "error", err.Error())
	}
}

// warnIfAudienceOverlap emits a WARN log if the registering provider p shares
// an empty or overlapping ExpectedAudiences with any prior/concurrent owner of
// the same URI. Called only when priors is non-empty (cross-tenant case).
//
// Logic: if two providers both have empty ExpectedAudiences (any-aud accepted),
// or if their ExpectedAudiences sets have a non-empty intersection, they are
// ambiguous and tokens will be rejected with ErrAmbiguousProvider. Operators
// must set DISTINCT ExpectedAudiences on each tenant's provider.
//
// We read each prior provider from the store under the system context to
// discover their ExpectedAudiences. Failures to read are non-fatal (WARN log
// is a best-effort signal, not a correctness gate).
func (s *Service) warnIfAudienceOverlap(ctx context.Context, p *OidcProvider, history *UriOwnershipHistory, uriHash string) {
	if history == nil {
		return
	}
	// Collect all active owners from history (CurrentOwner + Past without DeletedAt).
	var owners []Owner
	if history.CurrentOwner != nil {
		owners = append(owners, *history.CurrentOwner)
	}
	for _, past := range history.Past {
		if past.DeletedAt == nil {
			owners = append(owners, past)
		}
	}

	for _, owner := range owners {
		if owner.ProviderUUID == p.ID.String() {
			// Skip self (the newly registering provider).
			continue
		}
		prior, err := s.store.Get(ctx, spi.TenantID(owner.TenantID), owner.ProviderUUID)
		if err != nil {
			// Non-fatal — cannot read prior provider's audiences; skip.
			continue
		}
		if audienceSetsOverlap(p.ExpectedAudiences, prior.ExpectedAudiences) {
			s.logger.Warn("oidc.cross_tenant_audience_overlap",
				"pkg", "oidc",
				"registering_tenant", p.OwnerLegalEntityID.String(),
				"prior_tenant", owner.TenantID,
				"prior_provider_uuid", owner.ProviderUUID,
				"wellknown_uri_hash", uriHash,
				"action_required", "set distinct expectedAudiences on each tenant's provider to avoid ErrAmbiguousProvider at validation time",
			)
		}
	}
}

// audienceSetsOverlap returns true if the two audience slices have a non-empty
// intersection, or if EITHER slice is empty (which means "accept any audience" —
// an implicit overlap with everything).
func audienceSetsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, ea := range a {
		for _, eb := range b {
			if ea == eb {
				return true
			}
		}
	}
	return false
}

// containsStr is a small string-slice helper for the audit-log priors list.
func containsStr(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
