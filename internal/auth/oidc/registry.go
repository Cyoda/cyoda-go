package oidc

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
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

// RegistryConfig holds the tunable parameters for a Registry instance. It
// consolidates what would otherwise be a long, growing positional argument list
// on NewRegistry.
type RegistryConfig struct {
	// AllowPrivateNetworks mirrors CYODA_OIDC_ALLOW_PRIVATE_NETWORKS: when
	// false, JWKS fetches are subject to the safeDialContext blocklist that
	// also applies at register-time (D10). Set to true only in test/dev
	// environments where the IdP runs on a loopback address.
	AllowPrivateNetworks bool

	// ConnectTimeout is applied as the TLSHandshakeTimeout on the JWKS-fetch
	// transport. Zero or negative values default to 5 s (matching the
	// DiscoveryConfig default in discovery.go).
	ConnectTimeout time.Duration

	// SocketTimeout is applied as the ResponseHeaderTimeout on the JWKS-fetch
	// transport. Zero or negative values default to 5 s.
	SocketTimeout time.Duration

	// WarmupRetryInterval is the tick interval of StartWarmupRetryLoop, which
	// re-attempts the JWKS warm-up of active providers that have no installed
	// key source (e.g. the IdP was unreachable at startup or registration).
	// Zero or negative values default to 30 s.
	WarmupRetryInterval time.Duration

	// ReconcileInterval is the tick interval of StartReconcileLoop, the
	// periodic KV-reconcile backstop behind the best-effort provider
	// broadcast. Zero or negative values default to 60 s (matching
	// CYODA_AUTH_CACHE_RECONCILE_INTERVAL). The per-tick wait is jittered
	// ±10% to avoid a cross-node herd.
	ReconcileInterval time.Duration
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
	metrics      Metrics
	logger       *slog.Logger
	cfg          RegistryConfig

	// warmupRetryStarted makes StartWarmupRetryLoop's call-once contract
	// self-enforcing: a second call is a no-op.
	warmupRetryStarted atomic.Bool

	// reconcileLoopStarted makes StartReconcileLoop's call-once contract
	// self-enforcing: a second call is a no-op. Also read by the ResolveKey
	// staleness breaker to know whether reconcile accounting is live.
	reconcileLoopStarted atomic.Bool

	// reloadWarmMu serializes ReloadAllAndWarm: concurrent force-warm calls
	// (an admin looping the reload endpoint, or endpoint + broadcast racing)
	// queue behind one fetch pool instead of multiplying outbound traffic to
	// every tenant's IdP.
	reloadWarmMu sync.Mutex

	// mapGen counts direct provider-map mutations AND ReloadAll swaps.
	// ReloadAll snapshots it before store.LoadAll and discards its built
	// snapshot if it changed by swap time — a mutation or a faster reload
	// landing mid-build always wins over the older snapshot.
	mapGen atomic.Uint64
	// reconcileMu serializes ReloadAll executions (periodic reconcile,
	// reload_all broadcast, admin endpoint) so overlapping rebuilds cannot
	// commit KV snapshots out of order.
	reconcileMu sync.Mutex

	// Reconcile health (read by the ResolveKey staleness breaker).
	lastReconcileNanos  atomic.Int64
	consecutiveFailures atomic.Int64

	// loadAllHook, when non-nil, runs after store.LoadAll inside ReloadAll.
	// Test-only seam for generation-guard races.
	loadAllHook func()
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
	cfg RegistryConfig,
) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = NopMetrics{}
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.SocketTimeout <= 0 {
		cfg.SocketTimeout = 5 * time.Second
	}
	if cfg.WarmupRetryInterval <= 0 {
		cfg.WarmupRetryInterval = 30 * time.Second
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 60 * time.Second
	}
	r := &Registry{
		providers:    map[spi.TenantID]map[string]*OidcProvider{},
		sources:      map[spi.TenantID]map[string]*providerSource{},
		kidIndex:     map[string][]providerRef{},
		store:        store,
		discovery:    disc,
		broadcast:    broadcast,
		singleflight: newSingleflightDebouncer(),
		metrics:      metrics,
		logger:       logger,
		cfg:          cfg,
	}
	if broadcast != nil {
		broadcast.Subscribe(topicOidcProviders, r.handleBroadcast)
	}
	return r
}

// addToProviderMap ensures p is present in r.providers under its own
// (OwnerLegalEntityID, WellKnownConfigURI) coordinate. Called by the Service
// write paths (Register, Reactivate) before reloadOne so that reloadOne's
// I9 guard does not silently skip the newly registered / reactivated provider.
//
// This is safe to call concurrently: the write lock is held for the minimum
// duration needed to mutate the nested maps.
func (r *Registry) addToProviderMap(p *OidcProvider) {
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers[tenant] == nil {
		r.providers[tenant] = map[string]*OidcProvider{}
	}
	r.providers[tenant][p.WellKnownConfigURI] = p
	r.mapGen.Add(1)
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
	r.mapGen.Add(1)
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
// aud is the token's audience claim (single string or first element extracted
// by the caller). It is used only in the multi-candidate disambiguation step
// (Layer 1 of the Critical audit fix, #284): when multiple providers are
// simultaneously iss-eligible and sig-verifying, ExpectedAudiences is used
// to route to the correct tenant. Pass an empty string when aud is absent.
//
// Hot path (RLock): if kidIndex has candidates for kid, run disposeCandidates
// immediately.
//
// Cold path (Lock, mutates kidIndex): iterate all providers globally in
// deterministic (tenant, uri) lexicographic order (Layer 2 — defense-in-depth),
// run disposeCandidates, and on success populate kidIndex for the next call.
//
// D6 invariant: kidIndex is populated BEFORE the caller verifies the
// signature. The caller MUST call EvictKidEntry on ErrSignatureFailure.
func (r *Registry) ResolveKey(kid, iss, aud string) (*KeyResolution, error) {
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
		res, err = r.disposeCandidates(candidates, kid, iss, aud)
	}()
	if err == nil || !errors.Is(err, auth.ErrUnknownKID) {
		return res, err
	}

	// Cold path under Lock for kidIndex mutation — re-iterate everything.
	// Layer 2: sort deterministically by (tenant, uri) so kidIndex population
	// order is reproducible across nodes and Go's randomized map iteration
	// cannot affect which candidate is appended first.
	r.mu.Lock()
	defer r.mu.Unlock()
	var allRefs []providerRef
	for tenant, byURI := range r.providers {
		for uri := range byURI {
			allRefs = append(allRefs, providerRef{tenant: tenant, uri: uri})
		}
	}
	sort.Slice(allRefs, func(i, j int) bool {
		if allRefs[i].tenant != allRefs[j].tenant {
			return allRefs[i].tenant < allRefs[j].tenant
		}
		return allRefs[i].uri < allRefs[j].uri
	})
	res, err = r.disposeCandidates(allRefs, kid, iss, aud)
	if err == nil && res != nil {
		// D6: populate kidIndex at resolution time, before sig check.
		//
		// When multiple providers across tenants share the same kid (same physical
		// IdP), we populate ALL key-eligible refs into kidIndex, not just the
		// winner. This ensures that subsequent hot-path calls for a DIFFERENT aud
		// see all candidates and can apply audience disambiguation correctly —
		// preventing permanent wrong-tenant routing after the first resolution.
		//
		// Dedup: concurrent cold-path goroutines for the same kid could each
		// resolve the same ref; skip appends for refs already present.
		keyRefs := r.collectKeyEligibleRefs(allRefs, kid, iss)
		existing := r.kidIndex[kid]
		for _, ref := range keyRefs {
			already := false
			for _, e := range existing {
				if e == ref {
					already = true
					break
				}
			}
			if !already {
				existing = append(existing, ref)
			}
		}
		r.kidIndex[kid] = existing
	}
	return res, err
}

// collectKeyEligibleRefs returns the set of providerRefs from candidates that
// are iss-eligible AND whose keySource returns a key for kid. Used by the cold
// path to populate kidIndex with ALL matching refs so subsequent hot-path calls
// with different aud values can apply audience disambiguation without re-walking
// all providers. Caller must hold the write lock.
func (r *Registry) collectKeyEligibleRefs(candidates []providerRef, kid, iss string) []providerRef {
	var out []providerRef
	for _, ref := range candidates {
		prov, ok := r.providers[ref.tenant][ref.uri]
		if !ok || !prov.Active() {
			continue
		}
		src, ok := r.sources[ref.tenant][ref.uri]
		if !ok || src.discoveryDoc == nil {
			continue
		}
		if !issMatches(prov, src.discoveryDoc, iss) {
			continue
		}
		_, err := src.keySource.GetKey(kid)
		if err != nil {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// disposeCandidates walks the candidate set and applies the iss-validation
// rule, then attempts source.GetKey on every iss-eligible candidate. Caller
// must hold the appropriate lock (RLock for hot path, Lock for cold path).
//
// aud is the token's primary audience claim (empty string if absent). When
// multiple candidates are key-eligible, audience disambiguation is applied:
//
//  1. Collect all iss-eligible candidates.
//  2. For each, attempt GetKey. Collect those that return a key (keyEligible).
//  3. If exactly one keyEligible: return it (single-match path unchanged).
//  4. If multiple keyEligible: disambiguate by aud.
//     - Collect audMatched = those whose ExpectedAudiences is non-empty AND
//     contains aud.
//     - Exactly one audMatched → return it.
//     - Zero or multiple audMatched → ErrAmbiguousProvider (wraps
//     ErrUnknownKID so the chain falls through). This prevents silent
//     cross-tenant routing when two tenants share an IdP without setting
//     distinct ExpectedAudiences (Critical audit fix, #284).
//
// Return semantics:
//   - success → KeyResolution with ProviderRef populated
//   - at least one iss-eligible candidate but all sources returned transient
//     errors → ErrJWKSUnavailable
//   - no iss-eligible candidates but at least one kid-matched candidate was
//     rejected by iss → ErrIssuerMismatch
//   - ambiguous (multiple key-eligible, no unique aud match) → ErrAmbiguousProvider
//   - otherwise → ErrUnknownKID
func (r *Registry) disposeCandidates(candidates []providerRef, kid, iss, aud string) (*KeyResolution, error) {
	if len(candidates) == 0 {
		return nil, auth.ErrUnknownKID
	}
	var hadIssRejected bool
	var lastTransientErr error

	// Phase 1: collect iss-eligible candidates.
	type keyEligibleEntry struct {
		ref  providerRef
		prov *OidcProvider
		pub  *rsa.PublicKey
	}
	var keyEligible []keyEligibleEntry

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
		keyEligible = append(keyEligible, keyEligibleEntry{ref: ref, prov: prov, pub: pub})
	}

	if lastTransientErr != nil && len(keyEligible) == 0 {
		return nil, auth.ErrJWKSUnavailable
	}

	switch len(keyEligible) {
	case 0:
		if lastTransientErr != nil {
			return nil, auth.ErrJWKSUnavailable
		}
		if hadIssRejected {
			// Had kid-matched candidates but all were rejected by iss check.
			return nil, auth.ErrIssuerMismatch
		}
		return nil, auth.ErrUnknownKID

	case 1:
		// Single match — return immediately (common path).
		e := keyEligible[0]
		return &KeyResolution{
			PublicKey:          e.pub,
			Provider:           e.prov,
			WellKnownConfigURI: e.ref.uri,
			ProviderRef:        e.ref,
		}, nil

	default:
		// Multiple key-eligible candidates across tenants. Disambiguate by aud.
		// Layer 1 of Critical audit fix (#284): prevents non-deterministic
		// cross-tenant routing when two tenants register the same IdP URL.
		var audMatched []keyEligibleEntry
		for _, e := range keyEligible {
			if len(e.prov.ExpectedAudiences) > 0 && audienceContains(e.prov.ExpectedAudiences, aud) {
				audMatched = append(audMatched, e)
			}
		}
		if len(audMatched) == 1 {
			e := audMatched[0]
			return &KeyResolution{
				PublicKey:          e.pub,
				Provider:           e.prov,
				WellKnownConfigURI: e.ref.uri,
				ProviderRef:        e.ref,
			}, nil
		}
		// Zero or multiple audMatched: reject to prevent silent cross-tenant routing.
		// Operators must set distinct ExpectedAudiences on each tenant's provider
		// to allow shared-IdP deployments.
		return nil, ErrAmbiguousProvider
	}
}

// audienceContains reports whether aud is a member of the expected slice.
// aud is the single-string audience extracted from the token (the caller is
// responsible for flattening []aud → first-match string before calling here;
// for the resolver path we compare against the raw aud string).
func audienceContains(expected []string, aud string) bool {
	for _, e := range expected {
		if e == aud {
			return true
		}
	}
	return false
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

// maxReloadAttempts bounds the generation-guard retry. Only continuous
// provider-map churn (rare admin operations) can exhaust it.
const maxReloadAttempts = 5

// errReloadContention marks a reload that gave up because mutations kept
// landing mid-rebuild. Not a KV failure; excluded from staleness accounting.
var errReloadContention = errors.New("oidc registry reload: retry budget exhausted under concurrent mutation")

// ReloadAll rebuilds the in-memory provider map from KV (D18). The new maps
// are built off-lock; the swap takes the write lock so no partial-rebuild
// state is ever visible to concurrent readers.
//
// Key sources survive the swap: every installed source whose (tenant, uri)
// coordinate is still present in the fresh store snapshot is carried over,
// so a reload is never a cache flush — tokens verified by a warm provider
// keep verifying even if no warm-up follows or its discovery fetch fails.
// Key freshness is still governed by the JWKS cache TTL (stale keys that
// cannot be re-confirmed stop validating — fail closed). Sources whose
// provider vanished from the store are dropped, and the kidIndex is rebuilt
// lazily by the ResolveKey cold path.
//
// ReloadAll executions are serialized by reconcileMu (periodic reconcile,
// reload_all broadcast, and the admin reload endpoint can all call in),
// and each attempt is guarded by mapGen: if a direct provider-map mutation
// (or a faster concurrent ReloadAll) lands between the KV load and the
// swap, the stale snapshot is discarded and the load is retried rather than
// clobbering the newer state. An identical snapshot (a quiet tick) skips
// the swap entirely so kidIndex and warm sources are not needlessly reset;
// it still prunes any orphaned sources and counts as a successful reconcile.
func (r *Registry) ReloadAll(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	for attempt := 0; attempt < maxReloadAttempts; attempt++ {
		gen := r.mapGen.Load()
		providers, err := r.store.LoadAll(ctx)
		if err != nil {
			r.noteReconcileFailure(err)
			return err
		}
		if r.loadAllHook != nil {
			r.loadAllHook()
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
		}

		done := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.mapGen.Load() != gen {
				return false
			}
			// Quiet tick: identical snapshot ⇒ skip the swap so kidIndex and
			// sources stay warm. reflect.DeepEqual false-negatives (e.g. a
			// locally-registered provider's monotonic-clock CreatedAt vs its
			// KV round-trip) merely cause one extra swap and then stabilize.
			if reflect.DeepEqual(newProv, r.providers) {
				r.pruneOrphanSourcesLocked()
				r.metrics.SetRegistryProviders(len(providers))
				return true
			}
			for tenant, byURI := range r.sources {
				for uri, src := range byURI {
					if _, still := newProv[tenant][uri]; still {
						newSrc[tenant][uri] = src
					}
				}
			}
			r.providers = newProv
			r.sources = newSrc
			r.kidIndex = map[string][]providerRef{}
			r.mapGen.Add(1)
			r.metrics.SetRegistryProviders(len(providers))
			return true
		}()
		if done {
			r.noteReconcileSuccess()
			return nil
		}
	}
	r.logger.Warn("oidc registry reload: giving up after repeated mid-rebuild mutations",
		"pkg", "oidc", "attempts", maxReloadAttempts)
	return errReloadContention
}

// pruneOrphanSourcesLocked drops sources whose provider is gone. A
// reloadOne install racing an earlier swap can strand one; the always-swap
// path used to collect it implicitly, the skip-swap path does it here.
// Caller must hold r.mu (write).
func (r *Registry) pruneOrphanSourcesLocked() {
	for tenant, byURI := range r.sources {
		for uri := range byURI {
			if _, ok := r.providers[tenant][uri]; !ok {
				delete(byURI, uri)
			}
		}
	}
}

func (r *Registry) noteReconcileSuccess() {
	r.lastReconcileNanos.Store(time.Now().UnixNano())
	r.consecutiveFailures.Store(0)
	r.metrics.SetReconcileConsecutiveFailures(0)
	r.metrics.SetReconcileStalenessSeconds(0)
}

func (r *Registry) noteReconcileFailure(err error) {
	n := r.consecutiveFailures.Add(1)
	r.metrics.SetReconcileConsecutiveFailures(int(n))
	r.metrics.SetReconcileStalenessSeconds(r.reconcileAge().Seconds())
	msg := "oidc provider reconcile failed; serving last-known providers until it succeeds"
	if n > errorEscalationThreshold {
		r.logger.Error(msg, "pkg", "oidc", "consecutiveFailures", n, "error", err.Error())
	} else {
		r.logger.Warn(msg, "pkg", "oidc", "consecutiveFailures", n, "error", err.Error())
	}
}

// reconcileAge returns time since the last successful ReloadAll.
func (r *Registry) reconcileAge() time.Duration {
	return time.Since(time.Unix(0, r.lastReconcileNanos.Load()))
}

// errorEscalationThreshold is the consecutive-failure count from which
// reconcile failures log at ERROR instead of WARN.
const errorEscalationThreshold = 3

// ReloadAllAndWarm is the force-warm reload used by the reload endpoint and
// the cluster reload_all broadcast: rebuild from KV, then synchronously
// re-fetch discovery + JWKS for every loaded active provider. A failed
// discovery fetch is non-fatal (WARN-logged by reloadOne) and leaves the
// carried-over source in place; a successful one installs a fresh source
// whose key freshness is governed by the standard JWKS cache TTL, fail-closed.
// Startup phase 1 (LoadProvidersFromKV) must NOT use this — the listener-bind
// ordering requires warm-up to stay async there.
//
// The warm phase is a cluster-visible operation triggered by an admin call;
// it runs detached from the caller's cancellation so an HTTP client
// disconnecting mid-request cannot leave it half-completed. Each fetch is
// individually bounded by the discovery/JWKS transport timeouts.
func (r *Registry) ReloadAllAndWarm(ctx context.Context) error {
	r.reloadWarmMu.Lock()
	defer r.reloadWarmMu.Unlock()
	if err := r.ReloadAll(ctx); err != nil {
		return err
	}
	r.WarmJWKS(context.WithoutCancel(ctx))
	return nil
}

// LoadProvidersFromKV is the Phase-1 startup alias for ReloadAll.
func (r *Registry) LoadProvidersFromKV(ctx context.Context) error {
	return r.ReloadAll(ctx)
}

// WarmJWKS fetches discovery + JWKS for every loaded ACTIVE provider through
// a bounded worker pool and blocks until the pool drains — callers that must
// not block (the phase-2 startup hook) wrap it in their own goroutine, and
// ReloadAllAndWarm relies on it being synchronous. Per-provider failures are
// WARN-logged and skipped. Invalidated providers are never fetched: their
// endpoints are explicitly distrusted, and reactivation re-warms explicitly.
func (r *Registry) WarmJWKS(ctx context.Context) {
	var refs []providerRef
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		for tenant, byURI := range r.providers {
			for uri, prov := range byURI {
				if !prov.Active() {
					continue
				}
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
				r.warmOneSafely(ctx, ref)
			}
		}()
	}
	for _, ref := range refs {
		jobs <- ref
	}
	close(jobs)
	wg.Wait()
}

// StartWarmupRetryLoop spawns the warm-up retry goroutine: every
// WarmupRetryInterval it re-attempts discovery + JWKS warm-up for active
// providers that have no installed key source (the one-shot startup warm-up
// or a registration-time warm-up lost the race against an IdP that was not
// yet reachable). The loop exits when ctx is cancelled. Called once at
// startup with a process-lifetime context; returns false (and starts
// nothing) if the loop is already running.
func (r *Registry) StartWarmupRetryLoop(ctx context.Context) bool {
	if !r.warmupRetryStarted.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		ticker := time.NewTicker(r.cfg.WarmupRetryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, ref := range r.coldActiveRefs() {
					r.warmOneSafely(ctx, ref)
					if !r.isCold(ref) {
						r.logger.Info("oidc: provider JWKS warmed by retry loop",
							"pkg", "oidc", "tenant", string(ref.tenant),
							"uri_hash", sha256Hex(ref.uri))
					}
				}
			}
		}
	}()
	return true
}

// StartReconcileLoop starts the periodic KV-reconcile backstop: every
// ReconcileInterval (jittered ±10%) the provider map is rebuilt from the
// authoritative store via ReloadAll, bounding the staleness a dropped
// broadcast can cause. NOT ReloadAllAndWarm: no per-tick outbound IdP
// traffic — JWKS key freshness stays governed by the per-source cache, and
// the warmup-retry loop warms any provider the reconcile discovers cold.
// Returns false (starting nothing) if already running; exits on ctx cancel.
func (r *Registry) StartReconcileLoop(ctx context.Context) bool {
	if !r.reconcileLoopStarted.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		for {
			timer := time.NewTimer(jitteredInterval(r.cfg.ReconcileInterval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				tickCtx, cancel := context.WithTimeout(ctx, r.cfg.ReconcileInterval)
				_ = r.ReloadAll(tickCtx) // failures logged/accounted inside
				cancel()
			}
		}
	}()
	return true
}

// jitteredInterval returns d × [0.9, 1.1).
func jitteredInterval(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

// warmOneSafely runs reloadOne with a recover layer: the warm pool's worker
// goroutines and the retry-loop goroutine have no caller to propagate a
// panic to, so an unrecovered panic there would crash the process. Same
// contract as safeDispatch on the broadcast path; log-only here — the
// broadcast panic counter stays broadcast-scoped.
func (r *Registry) warmOneSafely(ctx context.Context, ref providerRef) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("oidc: warm-up panic",
				"pkg", "oidc", "tenant", string(ref.tenant),
				"uri_hash", sha256Hex(ref.uri), "panic", rec)
		}
	}()
	r.reloadOne(ctx, ref.tenant, ref.uri)
}

// coldActiveRefs returns the coordinates of active providers whose key
// source is missing or whose discovery doc never arrived — the set the
// warm-up retry loop re-attempts.
func (r *Registry) coldActiveRefs() []providerRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var refs []providerRef
	for tenant, byURI := range r.providers {
		for uri, prov := range byURI {
			if !prov.Active() {
				continue
			}
			src := r.sources[tenant][uri]
			if src == nil || src.discoveryDoc == nil {
				refs = append(refs, providerRef{tenant: tenant, uri: uri})
			}
		}
	}
	return refs
}

// isCold reports whether ref still lacks an installed source.
func (r *Registry) isCold(ref providerRef) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.sources[ref.tenant][ref.uri]
	return src == nil || src.discoveryDoc == nil
}

// reloadOne fetches discovery + JWKS for one (tenant, uri) provider and
// installs/updates the cached source. Used by both startup warmup and the
// broadcast-handler reload path.
//
// I9: if the provider is not present in r.providers at lookup time, log
// INFO + increment counter + return. Do NOT auto-create a provider entry.
func (r *Registry) reloadOne(ctx context.Context, tenant spi.TenantID, uri string) {
	r.reloadOneInternal(ctx, tenant, uri, true /* syncKeys */)
}

// reloadDiscoveryOnly re-fetches the discovery document for (tenant, uri) and
// updates the cached discoveryDoc WITHOUT replacing the existing keySource.
// Previously-cached JWKS keys remain in service. Used by Service.Reactivate
// when ReactivateKeys=false (D19 §2 spec: discovery-only reload).
func (r *Registry) reloadDiscoveryOnly(ctx context.Context, tenant spi.TenantID, uri string) {
	r.reloadOneInternal(ctx, tenant, uri, false /* syncKeys */)
}

// reloadOneInternal is the shared body for reloadOne and reloadDiscoveryOnly.
//
// When syncKeys=true (default): a fresh JWKS-backed keySource is constructed
// and installed together with the new discovery doc — replacing both.
//
// When syncKeys=false: the discovery doc is refreshed (I9 guard + pin check
// still apply) but the existing keySource is preserved. If no source is
// currently installed (Phase-2-pending), keySource remains nil; ResolveKey
// will return ErrUnknownKID until a full reloadOne is triggered.
func (r *Registry) reloadOneInternal(ctx context.Context, tenant spi.TenantID, uri string, syncKeys bool) {
	doc, err := r.discovery.Fetch(ctx, uri)
	if err != nil {
		r.logger.Warn("oidc: discovery fetch failed",
			"pkg", "oidc", "tenant", string(tenant),
			"uri_hash", sha256Hex(uri), "error", err.Error())
		r.metrics.IncJWKSFetchError("discovery")
		return
	}

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

	// E2 fetch-time pin enforcement: when the admin has pinned Issuers, the
	// discovery doc's issuer field MUST be in that list. An IdP returning a
	// mismatching issuer is either misconfigured or attacker-controlled; in
	// either case, refuse to install a source AND drop any existing one —
	// unlike a transient fetch failure, this is an affirmative conflict with
	// the binding the cached keys were fetched under, so keeping them serving
	// would fail open. The provider ends keyless — ResolveKey returns
	// ErrUnknownKID until the admin reconciles. Raw issuer strings are never
	// logged (A1+B1 lessons); only SHA-256 hashes and counts are emitted.
	if len(prov.Issuers) > 0 {
		issMatchesPin := false
		for _, allowed := range prov.Issuers {
			if allowed == doc.Issuer {
				issMatchesPin = true
				break
			}
		}
		if !issMatchesPin {
			r.logger.Warn("oidc: discovery doc issuer mismatch with pinned Issuers — refusing to install source",
				"pkg", "oidc",
				"tenant", string(tenant),
				"uri_hash", sha256Hex(uri),
				"doc_issuer_hash", sha256Hex(doc.Issuer),
				"pinned_count", len(prov.Issuers),
			)
			r.metrics.IncJWKSFetchError("issuer_pin_mismatch")
			func() {
				r.mu.Lock()
				defer r.mu.Unlock()
				if byURI, ok := r.sources[tenant]; ok {
					delete(byURI, uri)
				}
			}()
			return
		}
	}

	// buildFreshKeySource constructs a new safeDialContext-backed lazy JWKS
	// source for the discovery doc. Hoisted into a closure so both the
	// syncKeys=true path and the syncKeys=false fallback (no existing source)
	// share identical SSRF/TLS/timeout configuration.
	//
	// SSRF defence (D10): the per-dial safeDialContext applies even when the
	// discovery doc's jwks_uri differs from the originally-validated
	// wellKnownConfigUri (e.g. an attacker-controlled server returns a doc
	// with jwks_uri: http://169.254.169.254/...). If the HTTP client follows
	// redirects, the dialer is re-invoked for each TCP dial regardless of
	// origin. TLS 1.3 is preserved: the MinVersion pin is set on
	// TLSClientConfig, not on the DialContext, so both constraints apply
	// independently. Timeouts are threaded from RegistryConfig so the JWKS
	// transport is consistent with the discovery transport.
	//
	// The returned source is wrapped with a lifecycle gate. The isActive
	// closure is called by disposeCandidates which already holds r.mu (RLock
	// on the hot path, Lock on the cold path); re-acquiring r.mu inside the
	// closure would deadlock (Go's sync.RWMutex is not re-entrant). Instead,
	// we rely on disposeCandidates' own prov.Active() check, which guards
	// the GetKey call before it is ever reached. The closure here is a
	// defence-in-depth no-op wrapper that keeps the providerKeySource
	// contract intact for callers that do not hold the lock.
	buildFreshKeySource := func() auth.KeySource {
		jwksTransport := &http.Transport{
			DialContext:           safeDialContext(r.cfg.AllowPrivateNetworks),
			TLSHandshakeTimeout:   r.cfg.ConnectTimeout,
			ResponseHeaderTimeout: r.cfg.SocketTimeout,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS13},
		}
		inner := auth.NewHTTPJWKSSource(doc.JWKSURI, doc.Issuer, 5*time.Minute,
			auth.WithJWKSTransport(jwksTransport))
		localProv := prov
		return newProviderKeySource(inner, func() bool {
			return localProv.Active()
		})
	}

	if syncKeys {
		ks := buildFreshKeySource()
		func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.sources[tenant] == nil {
				r.sources[tenant] = map[string]*providerSource{}
			}
			r.sources[tenant][uri] = &providerSource{keySource: ks, discoveryDoc: doc}
		}()
		return
	}

	// syncKeys=false: refresh the discovery doc only. If an existing source
	// is installed, its keySource is preserved so previously-cached JWKS keys
	// remain in service without a JWKS fetch. If no source exists (e.g. after
	// Invalidate dropped it), fall back to building a fresh lazy source —
	// "preserve existing keys" degrades to "build empty cache" when there is
	// nothing to preserve, which is behaviourally identical. Installing nil
	// would later panic in disposeCandidates' GetKey call.
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sources[tenant] == nil {
			r.sources[tenant] = map[string]*providerSource{}
		}
		existing := r.sources[tenant][uri]
		var ks auth.KeySource
		if existing != nil && existing.keySource != nil {
			ks = existing.keySource
		} else {
			ks = buildFreshKeySource()
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
	r.mapGen.Add(1)
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
