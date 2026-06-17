package oidc

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// providerRef points to one provider entry by its (tenant, uri) coordinate.
// Used as the value type in kidIndex so the cold path can populate without
// holding pointers (which would race during reload_all rebuilds).
type providerRef struct {
	tenant spi.TenantID
	uri    string
}

// KeyResolution is returned by ResolveKey on success. The caller (OIDCValidator)
// is responsible for invoking EvictKidEntry on ErrSignatureFailure per D6.
//
// D6 invariant: kidIndex is populated at resolution time (cold path), BEFORE
// the caller verifies signatures. The caller MUST invoke EvictKidEntry on
// any signature failure — this self-heals the index for the next call.
type KeyResolution struct {
	PublicKey          *rsa.PublicKey
	Provider           *OidcProvider
	WellKnownConfigURI string
	ProviderRef        providerRef
}

// providerSource bundles the cached DiscoveryDoc and its derived KeySource.
type providerSource struct {
	keySource    auth.KeySource
	discoveryDoc *DiscoveryDoc
}

// Registry is the per-process OIDC provider cache. It implements the read
// path for OIDCValidator (ResolveKey) and the cluster-broadcast receive path
// (handleBroadcast — wired in broadcast.go).
type Registry struct {
	mu        sync.RWMutex
	providers map[spi.TenantID]map[string]*OidcProvider // by wellKnownConfigUri
	sources   map[spi.TenantID]map[string]*providerSource
	kidIndex  map[string][]providerRef // kid → candidate refs

	store        OidcProviderStore
	discovery    Discovery
	broadcast    spi.ClusterBroadcaster
	singleflight *singleflightDebouncer
	clock        func() time.Time
	metrics      Metrics
	logger       *slog.Logger
}

// NewRegistry constructs the registry. broadcast may be nil in tests or
// single-node deployments; the production startup hook validates non-nil
// when cluster mode is enabled.
func NewRegistry(
	store OidcProviderStore,
	disc Discovery,
	broadcast spi.ClusterBroadcaster,
	metrics Metrics,
	logger *slog.Logger,
) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = NopMetrics{}
	}
	r := &Registry{
		providers:    map[spi.TenantID]map[string]*OidcProvider{},
		sources:      map[spi.TenantID]map[string]*providerSource{},
		kidIndex:     map[string][]providerRef{},
		store:        store,
		discovery:    disc,
		broadcast:    broadcast,
		singleflight: newSingleflightDebouncer(),
		clock:        time.Now,
		metrics:      metrics,
		logger:       logger,
	}
	if broadcast != nil {
		broadcast.Subscribe(topicOidcProviders, r.handleBroadcast)
	}
	return r
}

// installForTest is a test-only helper that injects a provider + source +
// discovery doc directly into the registry, bypassing the discovery+JWKS
// fetch pipeline. Production code path is reloadOne.
func (r *Registry) installForTest(p *OidcProvider, ks auth.KeySource, doc *DiscoveryDoc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	if r.providers[tenant] == nil {
		r.providers[tenant] = map[string]*OidcProvider{}
	}
	if r.sources[tenant] == nil {
		r.sources[tenant] = map[string]*providerSource{}
	}
	r.providers[tenant][p.WellKnownConfigURI] = p
	r.sources[tenant][p.WellKnownConfigURI] = &providerSource{keySource: ks, discoveryDoc: doc}
}

// kidIndexContains is a test-only inspector for the kidIndex contents.
func (r *Registry) kidIndexContains(kid, tenant, uri string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, ref := range r.kidIndex[kid] {
		if string(ref.tenant) == tenant && ref.uri == uri {
			return true
		}
	}
	return false
}

// ResolveKey implements the §4.1 disposition matrix.
//
// Hot path (RLock): if kidIndex has candidates for kid, run disposeCandidates
// immediately.
//
// Cold path (Lock, mutates kidIndex): iterate all providers globally,
// run disposeCandidates, and on success populate kidIndex for the next call.
//
// D6 invariant: kidIndex is populated BEFORE the caller verifies the
// signature. The caller MUST call EvictKidEntry on ErrSignatureFailure.
func (r *Registry) ResolveKey(kid, iss string) (*KeyResolution, error) {
	// Hot path under RLock.
	var candidates []providerRef
	var res *KeyResolution
	var err error
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		candidates = r.kidIndex[kid]
		if len(candidates) > 0 {
			r.metrics.IncKidCacheHit()
		} else {
			r.metrics.IncKidCacheMiss()
		}
		res, err = r.disposeCandidates(candidates, kid, iss)
	}()
	if err == nil || !errors.Is(err, auth.ErrUnknownKID) {
		return res, err
	}

	// Cold path under Lock for kidIndex mutation — re-iterate everything.
	r.mu.Lock()
	defer r.mu.Unlock()
	var allRefs []providerRef
	for tenant, byURI := range r.providers {
		for uri := range byURI {
			allRefs = append(allRefs, providerRef{tenant: tenant, uri: uri})
		}
	}
	res, err = r.disposeCandidates(allRefs, kid, iss)
	if err == nil && res != nil {
		// D6: populate kidIndex at resolution time, before sig check.
		r.kidIndex[kid] = append(r.kidIndex[kid], res.ProviderRef)
	}
	return res, err
}

// disposeCandidates walks the candidate set and applies the iss-validation
// rule, then attempts source.GetKey on every iss-eligible candidate. Caller
// must hold the appropriate lock (RLock for hot path, Lock for cold path).
//
// Return semantics:
//   - success → KeyResolution with ProviderRef populated
//   - at least one iss-eligible candidate but all sources returned transient
//     errors → ErrJWKSUnavailable
//   - no iss-eligible candidates but at least one kid-matched candidate was
//     rejected by iss → ErrIssuerMismatch
//   - otherwise → ErrUnknownKID
func (r *Registry) disposeCandidates(candidates []providerRef, kid, iss string) (*KeyResolution, error) {
	if len(candidates) == 0 {
		return nil, auth.ErrUnknownKID
	}
	var hadIssEligible bool
	var hadIssRejected bool
	var lastTransientErr error
	for _, ref := range candidates {
		prov, ok := r.providers[ref.tenant][ref.uri]
		if !ok || !prov.Active() {
			continue
		}
		src, ok := r.sources[ref.tenant][ref.uri]
		if !ok || src.discoveryDoc == nil {
			// Phase-2-pending (D8): discovery not yet complete — this candidate
			// contributes nothing. Do not surface ErrIssuerMismatch.
			continue
		}
		// D17 mandatory bytewise iss check.
		if !issMatches(prov, src.discoveryDoc, iss) {
			hadIssRejected = true
			continue
		}
		hadIssEligible = true
		pub, err := src.keySource.GetKey(kid)
		if err != nil {
			if errors.Is(err, auth.ErrKeyNotFound) {
				// Hard miss from this source — keep iterating.
				continue
			}
			// Transient error (network, etc.) — record and keep iterating.
			lastTransientErr = err
			continue
		}
		return &KeyResolution{
			PublicKey:          pub,
			Provider:           prov,
			WellKnownConfigURI: ref.uri,
			ProviderRef:        ref,
		}, nil
	}
	if lastTransientErr != nil {
		return nil, auth.ErrJWKSUnavailable
	}
	if hadIssEligible {
		// Had iss-eligible candidates but all returned ErrKeyNotFound.
		return nil, auth.ErrUnknownKID
	}
	if hadIssRejected {
		// Had kid-matched candidates but all were rejected by iss check.
		return nil, auth.ErrIssuerMismatch
	}
	return nil, auth.ErrUnknownKID
}

// issMatches applies D17's strict bytewise iss-comparison rule.
// If provider.Issuers is non-empty, iss must be in the pin list.
// Otherwise iss must equal the discovery doc's Issuer field.
func issMatches(p *OidcProvider, doc *DiscoveryDoc, iss string) bool {
	if len(p.Issuers) > 0 {
		for _, allowed := range p.Issuers {
			if allowed == iss {
				return true
			}
		}
		return false
	}
	return iss == doc.Issuer
}

// EvictKidEntry removes ref from kidIndex[kid] (D6 self-heal). Idempotent:
// safe to call even if the entry has already been evicted by a concurrent caller.
func (r *Registry) EvictKidEntry(kid string, ref providerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.kidIndex[kid]
	out := list[:0]
	for _, e := range list {
		if e == ref {
			r.metrics.IncKidCacheEvict()
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		delete(r.kidIndex, kid)
	} else {
		r.kidIndex[kid] = out
	}
}

// ReloadAll rebuilds the in-memory provider map from KV (D18). The new maps
// are built off-lock; the swap takes the write lock so no partial-rebuild
// state is ever visible to concurrent readers.
func (r *Registry) ReloadAll(ctx context.Context) error {
	providers, err := r.store.LoadAll(ctx)
	if err != nil {
		return err
	}

	// Build fresh maps off-lock so the critical section is just the swap.
	newProv := map[spi.TenantID]map[string]*OidcProvider{}
	newSrc := map[spi.TenantID]map[string]*providerSource{}
	for _, p := range providers {
		tenant := spi.TenantID(p.OwnerLegalEntityID.String())
		if newProv[tenant] == nil {
			newProv[tenant] = map[string]*OidcProvider{}
			newSrc[tenant] = map[string]*providerSource{}
		}
		newProv[tenant][p.WellKnownConfigURI] = p
		// Sources are populated by Phase-2 warmup / individual reloadOne calls.
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = newProv
	r.sources = newSrc
	r.kidIndex = map[string][]providerRef{}
	r.metrics.SetRegistryProviders(len(providers))
	return nil
}

// LoadProvidersFromKV is the Phase-1 startup alias for ReloadAll.
func (r *Registry) LoadProvidersFromKV(ctx context.Context) error {
	return r.ReloadAll(ctx)
}

// WarmJWKSAsync is Phase-2 startup warmup. It spawns a bounded worker pool
// that fetches discovery + JWKS for every loaded provider. Per-provider
// failures are WARN-logged; the goroutine does not block startup.
func (r *Registry) WarmJWKSAsync(ctx context.Context) {
	var refs []providerRef
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		for tenant, byURI := range r.providers {
			for uri := range byURI {
				refs = append(refs, providerRef{tenant: tenant, uri: uri})
			}
		}
	}()

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan providerRef, len(refs))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range jobs {
				r.reloadOne(ctx, ref.tenant, ref.uri)
			}
		}()
	}
	for _, ref := range refs {
		jobs <- ref
	}
	close(jobs)
	wg.Wait()
}

// reloadOne fetches discovery + JWKS for one (tenant, uri) provider and
// installs/updates the cached source. Used by both startup warmup and the
// broadcast-handler reload path.
//
// I9: if the provider is not present in r.providers at lookup time, log
// INFO + increment counter + return. Do NOT auto-create a provider entry.
func (r *Registry) reloadOne(ctx context.Context, tenant spi.TenantID, uri string) {
	doc, err := r.discovery.Fetch(ctx, uri)
	if err != nil {
		r.logger.Warn("oidc: discovery fetch failed",
			"pkg", "oidc", "tenant", string(tenant),
			"uri_hash", sha256Hex(uri), "error", err.Error())
		r.metrics.IncJWKSFetchError("discovery")
		return
	}

	// Build the per-provider key source before taking any lock.
	inner := auth.NewHTTPJWKSSource(doc.JWKSURI, doc.Issuer, 5*time.Minute)

	// I9: check that the provider exists before installing the source.
	var prov *OidcProvider
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if byURI, ok := r.providers[tenant]; ok {
			prov = byURI[uri]
		}
	}()
	if prov == nil {
		r.logger.Info("oidc: broadcast for unknown provider",
			"pkg", "oidc", "tenant", string(tenant), "uri_hash", sha256Hex(uri))
		r.metrics.IncUnknownProviderBroadcast()
		return
	}

	// Wrap with lifecycle gate (reads provider map under RLock at call time).
	ks := newProviderKeySource(inner, func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if byURI, ok := r.providers[tenant]; ok {
			if p, ok := byURI[uri]; ok {
				return p.Active()
			}
		}
		return false
	})

	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sources[tenant] == nil {
			r.sources[tenant] = map[string]*providerSource{}
		}
		r.sources[tenant][uri] = &providerSource{keySource: ks, discoveryDoc: doc}
	}()
}

// invalidateOne drops the provider entry + its source and evicts all
// kidIndex entries pointing to this (tenant, uri). Used by the broadcast
// invalidate path.
func (r *Registry) invalidateOne(tenant spi.TenantID, uri string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if byURI, ok := r.providers[tenant]; ok {
		delete(byURI, uri)
	}
	if byURI, ok := r.sources[tenant]; ok {
		delete(byURI, uri)
	}
	target := providerRef{tenant: tenant, uri: uri}
	for kid, refs := range r.kidIndex {
		out := refs[:0]
		for _, ref := range refs {
			if ref == target {
				continue
			}
			out = append(out, ref)
		}
		if len(out) == 0 {
			delete(r.kidIndex, kid)
		} else {
			r.kidIndex[kid] = out
		}
	}
}
