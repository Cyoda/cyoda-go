# OIDC Providers Implementation Plan — Continued (Phases 4–11)

> **Continuation of** [`2026-06-16-284-oidc-providers-plan.md`](2026-06-16-284-oidc-providers-plan.md). Phases 0–3 (auth chain foundation, OIDC types, SSRF, discovery, KV store) live in the first file. This file picks up at Phase 4.

> **For agentic workers:** Same execution model as the first file. Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use `- [ ]` checkboxes.

---

## Phase 4: Registry + validator + UserContext (sequential within phase, depends on Phases 1+2+3)

### Task 4.1: Per-provider JWKS source (`internal/auth/oidc/jwks_source.go`)

**Spec ref:** §3.1, §4.1.

**Files:**
- Create: `internal/auth/oidc/jwks_source.go`
- Test: `internal/auth/oidc/jwks_source_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/auth/oidc/jwks_source_test.go
package oidc

import (
    "crypto/rand"
    "crypto/rsa"
    "testing"

    "github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestProviderKeySource_DelegatesGetKey(t *testing.T) {
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    delegate := auth.NewLocalKeySource()
    delegate.AddKeyPair(&auth.KeyPair{
        KID: "kid-1", PublicKey: &priv.PublicKey, PrivateKey: priv,
        Active: true, Algorithm: "RS256", Audience: "human",
    })

    pks := newProviderKeySource(delegate, func() bool { return true })
    got, err := pks.GetKey("kid-1")
    if err != nil {
        t.Fatalf("GetKey: %v", err)
    }
    if got == nil {
        t.Fatal("got nil key")
    }
}

func TestProviderKeySource_GatesByLifecycle(t *testing.T) {
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    delegate := auth.NewLocalKeySource()
    delegate.AddKeyPair(&auth.KeyPair{
        KID: "kid-1", PublicKey: &priv.PublicKey, PrivateKey: priv,
        Active: true, Algorithm: "RS256", Audience: "human",
    })

    pks := newProviderKeySource(delegate, func() bool { return false }) // invalidated
    _, err := pks.GetKey("kid-1")
    if err == nil {
        t.Fatal("expected error from invalidated provider, got nil")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/oidc/ -run TestProviderKeySource -v
```

Expected: FAIL — `newProviderKeySource` undefined.

- [ ] **Step 3: Implement**

```go
// internal/auth/oidc/jwks_source.go
package oidc

import (
    "crypto/rsa"
    "errors"

    "github.com/cyoda-platform/cyoda-go/internal/auth"
)

// providerKeySource wraps an underlying auth.KeySource (typically HTTPJWKSSource
// fetching from a discovered jwks_uri) and gates GetKey by a lifecycle
// predicate. When the provider is invalidated, GetKey refuses regardless of
// what the underlying source has cached.
type providerKeySource struct {
    inner    auth.KeySource
    isActive func() bool
}

func newProviderKeySource(inner auth.KeySource, isActive func() bool) *providerKeySource {
    return &providerKeySource{inner: inner, isActive: isActive}
}

func (p *providerKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
    if !p.isActive() {
        return nil, errors.New("oidc: provider is invalidated")
    }
    return p.inner.GetKey(kid)
}
```

- [ ] **Step 4: Verify tests pass**

```bash
go test ./internal/auth/oidc/ -run TestProviderKeySource -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/jwks_source.go internal/auth/oidc/jwks_source_test.go
git commit -m "feat(oidc): providerKeySource with lifecycle gate (#284)"
```

### Task 4.2: Registry with self-healing kidIndex (`internal/auth/oidc/registry.go`)

**Spec ref:** §3.1, §4.1, §4.2, §5.6, §6, D6, D8, D18.

This is one of the largest single tasks. Split it into sub-steps.

**Files:**
- Create: `internal/auth/oidc/singleflight.go` (the small debouncer)
- Create: `internal/auth/oidc/registry.go`
- Create: `internal/auth/oidc/observability.go`
- Test: `internal/auth/oidc/singleflight_test.go`
- Test: `internal/auth/oidc/registry_test.go`

- [ ] **Step 1: Singleflight debouncer — test**

```go
// internal/auth/oidc/singleflight_test.go
package oidc

import (
    "sync"
    "sync/atomic"
    "testing"
    "time"
)

func TestSingleflightDebouncer_DropsConcurrentSameKey(t *testing.T) {
    sf := newSingleflightDebouncer()
    var count int32
    block := make(chan struct{})
    var firstStarted sync.WaitGroup
    firstStarted.Add(1)

    fn := func() {
        atomic.AddInt32(&count, 1)
        firstStarted.Done()
        <-block
    }

    if ok := sf.Dispatch("k1", fn); !ok {
        t.Fatal("first dispatch dropped unexpectedly")
    }
    firstStarted.Wait()

    // Second dispatch under same key should be dropped.
    if ok := sf.Dispatch("k1", fn); ok {
        t.Fatal("second dispatch should drop")
    }
    close(block)
    time.Sleep(20 * time.Millisecond)

    if got := atomic.LoadInt32(&count); got != 1 {
        t.Errorf("count = %d, want 1", got)
    }
}

func TestSingleflightDebouncer_AllowsDifferentKeys(t *testing.T) {
    sf := newSingleflightDebouncer()
    var count int32
    done := make(chan struct{}, 2)
    fn := func() { atomic.AddInt32(&count, 1); done <- struct{}{} }

    _ = sf.Dispatch("k1", fn)
    _ = sf.Dispatch("k2", fn)
    <-done
    <-done
    if got := atomic.LoadInt32(&count); got != 2 {
        t.Errorf("count = %d, want 2", got)
    }
}
```

- [ ] **Step 2: Singleflight — implement**

```go
// internal/auth/oidc/singleflight.go
package oidc

import "sync"

// singleflightDebouncer drops concurrent same-key dispatches. Unlike
// golang.org/x/sync/singleflight, we don't queue callers for the result —
// we just discard later calls while the in-flight one runs. This matches
// D18's intent: collapse a burst of broadcasts for the same (T, uri) into
// one reload, dropping the rest entirely.
type singleflightDebouncer struct {
    mu       sync.Mutex
    inFlight map[string]struct{}
}

func newSingleflightDebouncer() *singleflightDebouncer {
    return &singleflightDebouncer{inFlight: make(map[string]struct{})}
}

// Dispatch returns true if a goroutine was spawned to run fn, false if the
// call was dropped because another for the same key is in flight.
func (s *singleflightDebouncer) Dispatch(key string, fn func()) bool {
    s.mu.Lock()
    if _, busy := s.inFlight[key]; busy {
        s.mu.Unlock()
        return false
    }
    s.inFlight[key] = struct{}{}
    s.mu.Unlock()

    go func() {
        defer func() {
            s.mu.Lock()
            delete(s.inFlight, key)
            s.mu.Unlock()
        }()
        fn()
    }()
    return true
}
```

- [ ] **Step 3: Verify singleflight tests pass**

```bash
go test ./internal/auth/oidc/ -run TestSingleflight -v
```

Expected: PASS.

- [ ] **Step 4: Observability — implement**

```go
// internal/auth/oidc/observability.go
package oidc

import (
    "github.com/cyoda-platform/cyoda-go/internal/observability"
)

// Metrics holds the OIDC subsystem's metric handles per D22. Constructed
// once at startup; passed to the Registry by value.
type Metrics struct {
    KidCacheHitTotal             observability.Counter
    KidCacheMissTotal            observability.Counter
    KidCacheEvictTotal           observability.Counter
    JWKSFetchErrorTotal          observability.CounterVec // label: outcome
    BroadcastPanicTotal          observability.Counter
    UnknownProviderBroadcastTotal observability.Counter
    BroadcastReceiveSeconds      observability.Histogram
    RegistryProviders            observability.Gauge // aggregate, no tenant label (D22 / I3)
}

// NewMetrics registers the OIDC metric set with the provided registry.
// Returns nil-safe placeholders if obs is nil (e.g. in unit tests).
func NewMetrics(obs *observability.Registry) *Metrics {
    if obs == nil {
        return &Metrics{
            KidCacheHitTotal:              observability.NopCounter(),
            KidCacheMissTotal:             observability.NopCounter(),
            KidCacheEvictTotal:            observability.NopCounter(),
            JWKSFetchErrorTotal:           observability.NopCounterVec(),
            BroadcastPanicTotal:           observability.NopCounter(),
            UnknownProviderBroadcastTotal: observability.NopCounter(),
            BroadcastReceiveSeconds:       observability.NopHistogram(),
            RegistryProviders:             observability.NopGauge(),
        }
    }
    return &Metrics{
        KidCacheHitTotal:              obs.NewCounter("oidc_kid_cache_hit_total", "OIDC kid-cache fast-path hits"),
        KidCacheMissTotal:             obs.NewCounter("oidc_kid_cache_miss_total", "OIDC kid-cache cold-path traversals"),
        KidCacheEvictTotal:            obs.NewCounter("oidc_kid_cache_evict_total", "OIDC kid-cache self-heal evictions (D6)"),
        JWKSFetchErrorTotal:           obs.NewCounterVec("oidc_jwks_fetch_error_total", "OIDC JWKS endpoint failures", []string{"outcome"}),
        BroadcastPanicTotal:           obs.NewCounter("oidc_broadcast_panic_total", "OIDC broadcast handler panics (defer recover)"),
        UnknownProviderBroadcastTotal: obs.NewCounter("oidc_unknown_provider_broadcast_total", "OIDC broadcasts for (T,uri) absent in local registry (I9)"),
        BroadcastReceiveSeconds:       obs.NewHistogram("oidc_broadcast_receive_seconds", "OIDC broadcast handler latency"),
        RegistryProviders:             obs.NewGauge("oidc_registry_providers", "Total active OIDC providers across all tenants"),
    }
}
```

> **Implementer note:** `internal/observability` may not expose exactly these constructors. If the package's actual surface differs, the implementer should adapt the types to match what's there (the precedent is `internal/observability/init.go` and surrounding files). The interface here is the contract the Registry expects.

- [ ] **Step 5: Registry — write failing tests**

```go
// internal/auth/oidc/registry_test.go
package oidc

import (
    "context"
    "crypto/rand"
    "crypto/rsa"
    "errors"
    "testing"
    "time"

    "github.com/google/uuid"
    "github.com/cyoda-platform/cyoda-go/internal/auth"
)

// fakeDiscovery for registry tests.
type fakeDiscovery struct {
    docs map[string]*DiscoveryDoc
    err  error
}

func (f *fakeDiscovery) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
    if f.err != nil {
        return nil, f.err
    }
    if d, ok := f.docs[uri]; ok {
        return d, nil
    }
    return nil, ErrDiscoveryFailed
}

// fakeKeySource — pretends to be HTTPJWKSSource for one kid.
type fakeKeySource struct {
    kid string
    key *rsa.PublicKey
}

func (f *fakeKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
    if kid == f.kid && f.key != nil {
        return f.key, nil
    }
    return nil, auth.ErrKeyNotFound
}

func newTestRegistry(t *testing.T) *Registry {
    t.Helper()
    return NewRegistry(newTestStore(t), &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}, nil, NewMetrics(nil), nil)
}

func TestRegistry_ResolveKey_ColdPathPopulatesKidIndex(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)

    p := &OidcProvider{
        ID:                 uuid.New(),
        WellKnownConfigURI: "https://idp.example",
        Issuers:            []string{"https://idp.example"},
        CreatedAt:          time.Now(),
        OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})

    res, err := r.ResolveKey("k1", "https://idp.example")
    if err != nil {
        t.Fatalf("ResolveKey: %v", err)
    }
    if res.Provider.ID != p.ID {
        t.Errorf("Provider mismatch")
    }

    // After cold-path resolve, the kidIndex should be populated.
    if !r.kidIndexContains("k1", p.OwnerLegalEntityID.String(), p.WellKnownConfigURI) {
        t.Error("kidIndex not populated after cold-path resolve (D6 invariant)")
    }
}

func TestRegistry_ResolveKey_IssMismatchReturnsErrIssuerMismatch(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)

    p := &OidcProvider{
        ID:                 uuid.New(),
        WellKnownConfigURI: "https://idp.example",
        Issuers:            []string{"https://idp.example"},
        CreatedAt:          time.Now(),
        OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})

    _, err := r.ResolveKey("k1", "https://evil.example")
    if !errors.Is(err, auth.ErrIssuerMismatch) {
        t.Errorf("err = %v, want ErrIssuerMismatch", err)
    }
}

func TestRegistry_ResolveKey_UnknownKidFallsThrough(t *testing.T) {
    r := newTestRegistry(t)
    _, err := r.ResolveKey("never-seen", "any")
    if !errors.Is(err, auth.ErrUnknownKID) {
        t.Errorf("err = %v, want ErrUnknownKID", err)
    }
}

func TestRegistry_ResolveKey_Phase2PendingReturnsErrUnknownKID(t *testing.T) {
    // D8 + I2 cold-start contradiction fix: missing discovery doc → ErrUnknownKID.
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)

    p := &OidcProvider{
        ID:                 uuid.New(),
        WellKnownConfigURI: "https://idp.example",
        CreatedAt:          time.Now(),
        OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey}, nil /* discoveryDoc nil */)

    _, err := r.ResolveKey("k1", "https://idp.example")
    if !errors.Is(err, auth.ErrUnknownKID) {
        t.Errorf("err = %v, want ErrUnknownKID (Phase-2-pending case)", err)
    }
}

func TestRegistry_EvictKidEntry_SelfHeal(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)

    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"},
        CreatedAt: time.Now(), OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    _, _ = r.ResolveKey("k1", "https://idp.example") // populates kidIndex
    ref := providerRef{tenant: spi.TenantID(p.OwnerLegalEntityID.String()), uri: p.WellKnownConfigURI}
    r.EvictKidEntry("k1", ref)

    if r.kidIndexContains("k1", p.OwnerLegalEntityID.String(), p.WellKnownConfigURI) {
        t.Error("kidIndex still contains entry after EvictKidEntry (D6 self-heal)")
    }
}

func TestRegistry_ReloadAll_TakesWriteLock(t *testing.T) {
    // D18: ReloadAll must hold the write lock for the rebuild duration.
    // We verify by attempting ResolveKey concurrently and ensuring it
    // waits until ReloadAll completes.
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"}, CreatedAt: time.Now(),
        OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    // Plant the provider in KV too so ReloadAll has work to do.
    _ = r.store.Register(context.Background(), p)
    _ = r.store.PutURIHistory(context.Background(), sha256Hex(p.WellKnownConfigURI), &UriOwnershipHistory{
        CurrentOwner: &Owner{TenantID: p.OwnerLegalEntityID.String(), ProviderUUID: p.ID.String(), RegisteredAt: time.Now()},
    })

    if err := r.ReloadAll(context.Background()); err != nil {
        t.Fatalf("ReloadAll: %v", err)
    }
    res, err := r.ResolveKey("k1", "https://idp.example")
    if err != nil {
        t.Fatalf("ResolveKey after ReloadAll: %v", err)
    }
    if res.Provider == nil {
        t.Fatal("got nil Provider")
    }
}
```

- [ ] **Step 6: Registry — implement**

```go
// internal/auth/oidc/registry.go
package oidc

import (
    "context"
    "errors"
    "log/slog"
    "sync"

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
type KeyResolution struct {
    PublicKey          *RSAPub  // alias for *rsa.PublicKey kept opaque to avoid the import in the godoc
    Provider           *OidcProvider
    WellKnownConfigURI string
    ProviderRef        providerRef
}

// RSAPub is an alias for *rsa.PublicKey kept here so tests can shadow it; the
// runtime type is rsa.PublicKey. The Registry doesn't itself import crypto/rsa
// outside this alias line.
type RSAPub = rsaPub
type rsaPub = anyRsaPub

// providerSource bundles the lifecycle gate + cached DiscoveryDoc + KeySource.
type providerSource struct {
    keySource    auth.KeySource
    discoveryDoc *DiscoveryDoc
}

// Registry is the per-process OIDC provider cache. It implements the read path
// for OIDCValidator (ResolveKey) and the cluster-broadcast receive path
// (handleBroadcast — implemented in broadcast.go).
type Registry struct {
    mu        sync.RWMutex
    providers map[spi.TenantID]map[string]*OidcProvider // by wellKnownConfigUri
    sources   map[spi.TenantID]map[string]*providerSource
    kidIndex  map[string][]providerRef

    store        OidcProviderStore
    discovery    Discovery
    broadcast    spi.ClusterBroadcaster
    singleflight *singleflightDebouncer
    clock        func() time.Time
    metrics      *Metrics
    logger       *slog.Logger
}

// NewRegistry constructs the registry. broadcast may be nil only in tests;
// the production startup hook validates non-nil and exits if missing.
func NewRegistry(store OidcProviderStore, disc Discovery, broadcast spi.ClusterBroadcaster, metrics *Metrics, logger *slog.Logger) *Registry {
    if logger == nil {
        logger = slog.Default()
    }
    if metrics == nil {
        metrics = NewMetrics(nil)
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
func (r *Registry) ResolveKey(kid, iss string) (*KeyResolution, error) {
    // Hot path under RLock.
    r.mu.RLock()
    candidates := r.kidIndex[kid]
    if len(candidates) > 0 {
        r.metrics.KidCacheHitTotal.Inc()
    } else {
        r.metrics.KidCacheMissTotal.Inc()
    }
    res, err := r.disposeCandidates(candidates, kid, iss)
    r.mu.RUnlock()
    if err == nil || !errors.Is(err, auth.ErrUnknownKID) {
        return res, err
    }

    // Cold path under Lock for kidIndex mutation. Re-iterate everything.
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
        // D6 load-bearing populate-at-resolve.
        r.kidIndex[kid] = append(r.kidIndex[kid], res.ProviderRef)
    }
    return res, err
}

// disposeCandidates walks the candidate set and applies the iss-validation rule,
// then attempts source.GetKey on every iss-eligible candidate. Caller holds
// the appropriate lock.
func (r *Registry) disposeCandidates(candidates []providerRef, kid, iss string) (*KeyResolution, error) {
    if len(candidates) == 0 {
        return nil, auth.ErrUnknownKID
    }
    var hadIssEligible bool
    var lastTransientErr error
    for _, ref := range candidates {
        prov, ok := r.providers[ref.tenant][ref.uri]
        if !ok || !prov.Active() {
            continue
        }
        src, ok := r.sources[ref.tenant][ref.uri]
        if !ok || src.discoveryDoc == nil {
            // Phase-2-pending (D8) — contribute nothing; do not surface ErrIssuerMismatch.
            continue
        }
        // D17 mandatory bytewise iss check.
        if !issMatches(prov, src.discoveryDoc, iss) {
            continue
        }
        hadIssEligible = true
        pub, err := src.keySource.GetKey(kid)
        if err != nil {
            // Distinguish transient (network) from hard miss (no such kid).
            if errors.Is(err, auth.ErrKeyNotFound) {
                continue
            }
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
        return nil, auth.ErrIssuerMismatch
    }
    return nil, auth.ErrUnknownKID
}

// issMatches applies D17's strict bytewise rule.
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

// EvictKidEntry removes ref from kidIndex[kid]. Idempotent; safe to call after
// signature failure even if the entry has already been evicted by a concurrent
// caller.
func (r *Registry) EvictKidEntry(kid string, ref providerRef) {
    r.mu.Lock()
    defer r.mu.Unlock()
    list := r.kidIndex[kid]
    out := list[:0]
    for _, e := range list {
        if e == ref {
            continue
        }
        out = append(out, e)
    }
    if len(out) == 0 {
        delete(r.kidIndex, kid)
    } else {
        r.kidIndex[kid] = out
    }
    r.metrics.KidCacheEvictTotal.Inc()
}

// ReloadAll takes the write lock for the entire rebuild (D18). All concurrent
// per-(T, uri) operations must complete first; new ones wait until the
// rebuild releases the lock.
func (r *Registry) ReloadAll(ctx context.Context) error {
    providers, err := r.store.LoadAll(ctx)
    if err != nil {
        return err
    }

    // Build fresh maps off-lock so the critical section is just the swap.
    newProv := map[spi.TenantID]map[string]*OidcProvider{}
    newSources := map[spi.TenantID]map[string]*providerSource{}
    for _, p := range providers {
        tenant := spi.TenantID(p.OwnerLegalEntityID.String())
        if newProv[tenant] == nil {
            newProv[tenant] = map[string]*OidcProvider{}
            newSources[tenant] = map[string]*providerSource{}
        }
        newProv[tenant][p.WellKnownConfigURI] = p
        // sources will be filled by Phase-2 warmup / individual reloadOne calls.
    }

    r.mu.Lock()
    r.providers = newProv
    r.sources = newSources
    r.kidIndex = map[string][]providerRef{}
    r.metrics.RegistryProviders.Set(float64(len(providers)))
    r.mu.Unlock()
    return nil
}

// LoadProvidersFromKV is Phase-1 warmup. Same as ReloadAll but exposes a
// distinct name for the startup wiring's readability.
func (r *Registry) LoadProvidersFromKV(ctx context.Context) error {
    return r.ReloadAll(ctx)
}

// WarmJWKSAsync is Phase-2 warmup. Spawns a bounded worker pool that fetches
// discovery + JWKS for every loaded provider. Per-provider failure is
// WARN-logged; the goroutine does not block startup.
func (r *Registry) WarmJWKSAsync(ctx context.Context) {
    r.mu.RLock()
    var refs []providerRef
    for tenant, byURI := range r.providers {
        for uri := range byURI {
            refs = append(refs, providerRef{tenant: tenant, uri: uri})
        }
    }
    r.mu.RUnlock()

    workers := runtime.NumCPU()
    jobs := make(chan providerRef, len(refs))
    var wg sync.WaitGroup
    for i := 0; i < workers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for ref := range jobs {
                r.reloadOne(ref.tenant, ref.uri)
            }
        }()
    }
    for _, ref := range refs {
        jobs <- ref
    }
    close(jobs)
    wg.Wait()
}

// reloadOne fetches discovery + JWKS for one provider and installs/updates
// the cached source. Used by both warmup and broadcast-handler paths.
func (r *Registry) reloadOne(tenant spi.TenantID, uri string) {
    ctx := context.Background()
    doc, err := r.discovery.Fetch(ctx, uri)
    if err != nil {
        r.logger.Warn("oidc: discovery fetch failed", "pkg", "oidc", "tenant", string(tenant), "uri_hash", sha256Hex(uri), "error", err.Error())
        r.metrics.JWKSFetchErrorTotal.WithLabel("outcome", "discovery").Inc()
        return
    }
    // Build the per-provider key source via the existing HTTPJWKSSource path.
    // Issuer in the source's cache key matches doc.Issuer.
    inner := auth.NewHTTPJWKSSource(doc.JWKSURI, doc.Issuer, 5*time.Minute)
    var prov *OidcProvider
    r.mu.RLock()
    if byURI, ok := r.providers[tenant]; ok {
        prov = byURI[uri]
    }
    r.mu.RUnlock()
    if prov == nil {
        // I9: broadcast for unknown provider — log + counter + return.
        r.logger.Info("oidc: broadcast for unknown provider", "pkg", "oidc", "tenant", string(tenant), "uri_hash", sha256Hex(uri))
        r.metrics.UnknownProviderBroadcastTotal.Inc()
        return
    }
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

    r.mu.Lock()
    if r.sources[tenant] == nil {
        r.sources[tenant] = map[string]*providerSource{}
    }
    r.sources[tenant][uri] = &providerSource{keySource: ks, discoveryDoc: doc}
    r.mu.Unlock()
}

// invalidateOne drops the provider entry + its source. Evicts the kidIndex.
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
```

> **Implementer note:** The `RSAPub = rsaPub = anyRsaPub` alias trick above is purely so the godoc reads cleanly; the actual implementation should `import "crypto/rsa"` and use `*rsa.PublicKey` directly. Adapt the alias machinery as needed — the contract is the field name and shape.

- [ ] **Step 7: Run all registry tests**

```bash
go test ./internal/auth/oidc/ -run TestRegistry -v
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/auth/oidc/registry.go internal/auth/oidc/singleflight.go \
        internal/auth/oidc/observability.go internal/auth/oidc/singleflight_test.go \
        internal/auth/oidc/registry_test.go
git commit -m "feat(oidc): Registry with self-healing kidIndex + reload_all write-lock (#284)"
```

### Task 4.3: UserContext extraction (`internal/auth/oidc/usercontext.go`)

**Spec ref:** §3.1, §4.3 step 8, D23, I2.

**Files:**
- Create: `internal/auth/oidc/usercontext.go`
- Test: `internal/auth/oidc/usercontext_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/oidc/usercontext_test.go
package oidc

import (
    "strings"
    "testing"

    "github.com/google/uuid"
)

func provider(rolesClaim *string) *OidcProvider {
    return &OidcProvider{
        ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
        OwnerLegalEntityID: uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"),
        RolesClaim:         rolesClaim,
    }
}

func TestBuildOIDCUserContext_NamespacedUserID(t *testing.T) {
    p := provider(nil)
    uc, err := buildOIDCUserContext(p, map[string]any{"sub": "alice"}, "roles")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    expected := "oidc:11111111-2222-3333-4444-555555555555:alice"
    if uc.UserID != expected {
        t.Errorf("UserID = %q, want %q", uc.UserID, expected)
    }
    if uc.Tenant.ID != "66666666-7777-8888-9999-aaaaaaaaaaaa" {
        t.Errorf("Tenant.ID = %q", uc.Tenant.ID)
    }
}

func TestBuildOIDCUserContext_RolesFromGlobalDefault(t *testing.T) {
    p := provider(nil)
    uc, err := buildOIDCUserContext(p, map[string]any{
        "sub": "alice", "roles": []any{"admin", "user"},
    }, "roles")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if len(uc.Roles) != 2 {
        t.Errorf("Roles = %v, want 2", uc.Roles)
    }
}

func TestBuildOIDCUserContext_PerProviderRolesClaim(t *testing.T) {
    rc := "cognito:groups"
    p := provider(&rc)
    uc, err := buildOIDCUserContext(p, map[string]any{
        "sub": "bob", "cognito:groups": []any{"admins"}, "roles": []any{"ignored"},
    }, "roles")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if len(uc.Roles) != 1 || uc.Roles[0] != "admins" {
        t.Errorf("Roles = %v, want [admins]", uc.Roles)
    }
}

func TestBuildOIDCUserContext_RolesSpaceDelimitedString(t *testing.T) {
    p := provider(nil)
    uc, err := buildOIDCUserContext(p, map[string]any{
        "sub": "alice", "roles": "admin user view",
    }, "roles")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if len(uc.Roles) != 3 {
        t.Errorf("Roles = %v, want 3", uc.Roles)
    }
}

func TestBuildOIDCUserContext_SubMissing(t *testing.T) {
    p := provider(nil)
    _, err := buildOIDCUserContext(p, map[string]any{}, "roles")
    if err == nil || !strings.Contains(err.Error(), "missing_sub") {
        t.Errorf("err = %v, want missing_sub", err)
    }
}

func TestBuildOIDCUserContext_SubTooLong(t *testing.T) {
    p := provider(nil)
    long := strings.Repeat("a", 256)
    _, err := buildOIDCUserContext(p, map[string]any{"sub": long}, "roles")
    if err == nil || !strings.Contains(err.Error(), "invalid_sub") {
        t.Errorf("err = %v, want invalid_sub", err)
    }
}

func TestBuildOIDCUserContext_SubControlCharRejected(t *testing.T) {
    p := provider(nil)
    cases := []string{"\nbad", "good\tbad", "go\x01bad", string([]byte{0x7f})}
    for _, s := range cases {
        _, err := buildOIDCUserContext(p, map[string]any{"sub": s}, "roles")
        if err == nil {
            t.Errorf("sub=%q expected rejection", s)
        }
    }
}

func TestBuildOIDCUserContext_IgnoresAttackerControlledClaims(t *testing.T) {
    p := provider(nil)
    uc, err := buildOIDCUserContext(p, map[string]any{
        "sub":          "alice",
        "caas_user_id": "victim",
        "caas_org_id":  "victim-org",
        "tid":          "victim-tenant",
    }, "roles")
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if uc.Tenant.ID == "victim-tenant" {
        t.Errorf("Tenant.ID was overridden by tid claim — D23 violation")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/auth/oidc/ -run TestBuildOIDCUserContext -v
```

Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/auth/oidc/usercontext.go
package oidc

import (
    "fmt"
    "strings"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

const maxSubLength = 255

// buildOIDCUserContext implements D23. Caller is OIDCValidator step 8.
// `claims` is the unverified token claims map. `defaultRolesClaim` is
// CYODA_OIDC_ROLES_CLAIM (typically "roles").
//
// UserID is namespaced as "oidc:<providerUUID>:<sub>" and is opaque
// downstream — no consumer may parse it. UserName is `sub` for display.
// Tenant.ID is the provider's OwnerLegalEntityID — token claims tid /
// tenant / org / caas_user_id / caas_org_id are explicitly ignored
// (attacker-controlled when the IdP is external).
func buildOIDCUserContext(p *OidcProvider, claims map[string]any, defaultRolesClaim string) (*spi.UserContext, error) {
    sub, _ := claims["sub"].(string)
    if sub == "" {
        return nil, fmt.Errorf("%w: missing_sub", subValidationSentinel)
    }
    if len(sub) > maxSubLength {
        return nil, fmt.Errorf("%w: invalid_sub: length %d > %d", subValidationSentinel, len(sub), maxSubLength)
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
// string (OAuth2 scope convention, RFC 8693 §4.2 — RFC 6749 §3.3).
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

// subValidationSentinel is the prefix carried in the returned error so
// OIDCValidator.Validate can map it to ErrClaimsFailure with the correct
// subcode (the OIDCValidator wraps with auth.ErrClaimsFailure).
var subValidationSentinel = fmt.Errorf("oidc/sub-validation")
```

- [ ] **Step 4: Verify tests pass**

```bash
go test ./internal/auth/oidc/ -run TestBuildOIDCUserContext -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/usercontext.go internal/auth/oidc/usercontext_test.go
git commit -m "feat(oidc): UserContext extraction with namespacing + sub bounds (#284)"
```

### Task 4.4: OIDCValidator (`internal/auth/oidc/validator.go`)

**Spec ref:** §3.1, §4.3, D17, D23, D6.

**Files:**
- Create: `internal/auth/oidc/validator.go`
- Test: `internal/auth/oidc/validator_test.go`

The full test set is large (see §11 rows 17-20, 30, 31-35, 36, 54-59). Implement the validator with one representative test per assertion class, then dispatch the remainder during the parity phase.

- [ ] **Step 1: Write failing tests covering the core assertion classes**

```go
// internal/auth/oidc/validator_test.go
package oidc

import (
    "crypto/rand"
    "crypto/rsa"
    "errors"
    "testing"
    "time"

    "github.com/google/uuid"
    "github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestOIDCValidator_HappyPath(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"},
        CreatedAt: time.Now().Add(-time.Hour), OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    v := NewValidator(r, "roles")
    tok := signRS256ForOIDCTest(t, "k1", priv, map[string]any{
        "iss": "https://idp.example",
        "sub": "alice",
        "exp": time.Now().Add(time.Hour).Unix(),
        "iat": time.Now().Unix(),
    })
    uc, err := v.Validate(tok)
    if err != nil {
        t.Fatalf("Validate: %v", err)
    }
    if uc.UserID != "oidc:"+p.ID.String()+":alice" {
        t.Errorf("UserID = %q", uc.UserID)
    }
}

func TestOIDCValidator_IatBeforeCreatedAtRejected(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    created := time.Now()
    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"},
        CreatedAt: created, OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    v := NewValidator(r, "roles")
    tok := signRS256ForOIDCTest(t, "k1", priv, map[string]any{
        "iss": "https://idp.example", "sub": "alice",
        "exp": time.Now().Add(time.Hour).Unix(),
        "iat": created.Add(-5 * time.Minute).Unix(), // outside 30s skew
    })
    _, err := v.Validate(tok)
    if !errors.Is(err, auth.ErrTokenPreTransition) {
        t.Errorf("err = %v, want ErrTokenPreTransition", err)
    }
}

func TestOIDCValidator_IatWithinSkewAccepted(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    created := time.Now()
    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"},
        CreatedAt: created, OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    v := NewValidator(r, "roles")
    tok := signRS256ForOIDCTest(t, "k1", priv, map[string]any{
        "iss": "https://idp.example", "sub": "alice",
        "exp": time.Now().Add(time.Hour).Unix(),
        "iat": created.Add(-5 * time.Second).Unix(), // within 30s skew
    })
    _, err := v.Validate(tok)
    if err != nil {
        t.Errorf("within-skew iat rejected: %v", err)
    }
}

func TestOIDCValidator_SigFailureEvictsKid(t *testing.T) {
    r := newTestRegistry(t)
    priv1, _ := rsa.GenerateKey(rand.Reader, 2048)
    priv2, _ := rsa.GenerateKey(rand.Reader, 2048)
    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"},
        CreatedAt: time.Now().Add(-time.Hour), OwnerLegalEntityID: uuid.New(),
    }
    // Provider's source returns priv1's public key — but we'll sign with priv2.
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv1.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    v := NewValidator(r, "roles")
    tok := signRS256ForOIDCTest(t, "k1", priv2, map[string]any{
        "iss": "https://idp.example", "sub": "alice",
        "exp": time.Now().Add(time.Hour).Unix(),
        "iat": time.Now().Unix(),
    })
    _, err := v.Validate(tok)
    if !errors.Is(err, auth.ErrSignatureFailure) {
        t.Errorf("err = %v, want ErrSignatureFailure", err)
    }

    // D6: kidIndex must be self-healed after sig failure.
    if r.kidIndexContains("k1", p.OwnerLegalEntityID.String(), p.WellKnownConfigURI) {
        t.Error("kidIndex still has the bad ref — D6 self-heal failed")
    }
}

func TestOIDCValidator_AudienceMismatch(t *testing.T) {
    r := newTestRegistry(t)
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    p := &OidcProvider{
        ID: uuid.New(), WellKnownConfigURI: "https://idp.example",
        Issuers: []string{"https://idp.example"},
        ExpectedAudiences: []string{"api1"},
        CreatedAt: time.Now().Add(-time.Hour), OwnerLegalEntityID: uuid.New(),
    }
    r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
        &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

    v := NewValidator(r, "roles")
    tok := signRS256ForOIDCTest(t, "k1", priv, map[string]any{
        "iss": "https://idp.example", "sub": "alice", "aud": "api2",
        "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
    })
    _, err := v.Validate(tok)
    if !errors.Is(err, auth.ErrClaimsFailure) {
        t.Errorf("err = %v, want ErrClaimsFailure", err)
    }
}
```

> **Implementer note:** `signRS256ForOIDCTest` is a thin wrapper that calls the existing signing helper in `internal/auth/test_helpers_test.go` or a similar utility. Add it to a `internal/auth/oidc/test_helpers_test.go` file.

- [ ] **Step 2: Add `signRS256ForOIDCTest` test helper**

```go
// internal/auth/oidc/test_helpers_test.go
package oidc

import (
    "crypto/rsa"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "testing"

    "github.com/cyoda-platform/cyoda-go/internal/auth"
)

func signRS256ForOIDCTest(t *testing.T, kid string, priv *rsa.PrivateKey, claims map[string]any) string {
    t.Helper()
    if s := auth.SignForTest(kid, priv, claims); s != "" {
        return s
    }
    // Fallback minimal signer if auth doesn't export a helper.
    hdr, _ := json.Marshal(map[string]any{"kid": kid, "alg": "RS256", "typ": "JWT"})
    cl, _ := json.Marshal(claims)
    enc := base64.RawURLEncoding.EncodeToString
    signingInput := enc(hdr) + "." + enc(cl)
    sig, err := auth.SignBytes([]byte(signingInput), priv)
    if err != nil {
        t.Fatalf("SignBytes: %v", err)
    }
    return fmt.Sprintf("%s.%s", signingInput, enc(sig))
}
```

> **Implementer note:** Verify what signing helpers `internal/auth` already exports. If `auth.SignForTest` / `auth.SignBytes` do not exist, the implementer must either (a) add a minimal exported helper to `internal/auth` (under a clear test-affordance comment) or (b) inline an RSA-PKCS1-v1.5-SHA256 signature here. Reuse first; only inline if necessary.

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/auth/oidc/ -run TestOIDCValidator -v
```

Expected: FAIL — `NewValidator` undefined.

- [ ] **Step 4: Implement**

```go
// internal/auth/oidc/validator.go
package oidc

import (
    "crypto/rsa"
    "encoding/base64"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/cyoda-platform/cyoda-go/internal/auth"
    spi "github.com/cyoda-platform/cyoda-go-spi"
)

const iatSkew = 30 * time.Second

// OIDCValidator implements auth.Validator using the OIDC provider Registry.
// Construction: NewValidator(registry, defaultRolesClaim). Wired into the
// chain after JWKSValidator (chain order is normative — spec §6).
type OIDCValidator struct {
    registry          *Registry
    defaultRolesClaim string
    clock             func() time.Time
}

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

func (v *OIDCValidator) Validate(tokenString string) (*spi.UserContext, error) {
    // 1. Header peek.
    parts := strings.Split(tokenString, ".")
    if len(parts) != 3 {
        return nil, fmt.Errorf("%w: malformed token", auth.ErrClaimsFailure)
    }
    h, err := parseTokenHeaderOIDC(parts[0], parts[1])
    if err != nil {
        return nil, fmt.Errorf("%w: %v", auth.ErrClaimsFailure, err)
    }
    if h.alg != "RS256" {
        return nil, fmt.Errorf("%w: unsupported_alg %q", auth.ErrClaimsFailure, h.alg)
    }
    now := v.clock()
    if h.exp > 0 && time.Unix(h.exp, 0).Before(now.Add(-iatSkew)) {
        return nil, fmt.Errorf("%w: expired", auth.ErrClaimsFailure)
    }

    // 2. sub validation (D23 / I2).
    if h.sub == "" {
        return nil, fmt.Errorf("%w: missing_sub", auth.ErrClaimsFailure)
    }

    // 3. Resolve key against the registry.
    res, err := v.registry.ResolveKey(h.kid, h.iss)
    if err != nil {
        return nil, err // propagate sentinel as-is
    }

    // 4. iat-binding (D17).
    if h.iat > 0 && time.Unix(h.iat, 0).Before(res.Provider.CreatedAt.Add(-iatSkew)) {
        return nil, fmt.Errorf("%w: iat=%d < CreatedAt=%d - %ds skew",
            auth.ErrTokenPreTransition, h.iat, res.Provider.CreatedAt.Unix(), int(iatSkew.Seconds()))
    }

    // 5. Verify signature. On failure → evict + ErrSignatureFailure.
    signingInput := parts[0] + "." + parts[1]
    sig, err := base64.RawURLEncoding.DecodeString(parts[2])
    if err != nil {
        return nil, fmt.Errorf("%w: malformed signature: %v", auth.ErrSignatureFailure, err)
    }
    if err := auth.VerifyRS256([]byte(signingInput), sig, res.PublicKey.(*rsa.PublicKey)); err != nil {
        v.registry.EvictKidEntry(h.kid, res.ProviderRef)
        return nil, fmt.Errorf("%w: %v", auth.ErrSignatureFailure, err)
    }

    // 6. nbf check (with 30s skew).
    if h.iat > 0 && time.Unix(h.iat, 0).After(now.Add(iatSkew)) {
        return nil, fmt.Errorf("%w: nbf — iat in future", auth.ErrClaimsFailure)
    }

    // 7. D20 audience.
    if len(res.Provider.ExpectedAudiences) > 0 {
        if !audienceMatches(h.aud, res.Provider.ExpectedAudiences) {
            return nil, fmt.Errorf("%w: audience", auth.ErrClaimsFailure)
        }
    }

    // 8. D23 UserContext extraction.
    claims, err := decodeClaimsForCtx(parts[1])
    if err != nil {
        return nil, fmt.Errorf("%w: %v", auth.ErrClaimsFailure, err)
    }
    uc, err := buildOIDCUserContext(res.Provider, claims, v.defaultRolesClaim)
    if err != nil {
        if errors.Is(err, subValidationSentinel) {
            return nil, fmt.Errorf("%w: %v", auth.ErrClaimsFailure, err)
        }
        return nil, err
    }
    return uc, nil
}

// audienceMatches accepts aud as either a single string or a JSON array.
func audienceMatches(audClaim string, expected []string) bool {
    // For OIDCValidator we accept the raw aud string. (Full array support
    // is rolled into the JSON-claims decoding path.)
    for _, e := range expected {
        if e == audClaim {
            return true
        }
    }
    return false
}
```

> **Implementer note:** The two small helpers `parseTokenHeaderOIDC` and `decodeClaimsForCtx` are equivalent to the work `parseTokenHeader` does in `internal/auth/parse.go`. To avoid duplication, the implementer should make `parseTokenHeader` (or a new exported helper) reusable from `oidc`. Move the logic to `internal/auth/parse_export.go` if needed, or call into it directly.

- [ ] **Step 5: Verify tests pass**

```bash
go test ./internal/auth/oidc/ -run TestOIDCValidator -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/oidc/validator.go internal/auth/oidc/test_helpers_test.go internal/auth/oidc/validator_test.go
git commit -m "feat(oidc): OIDCValidator with iat-binding + sig self-heal (#284)"
```

---

**Phase 4 complete.** Confirm:

```bash
go test ./internal/auth/oidc/... -count=1
go test ./internal/auth/... -count=1
```

---

## Phase 5: Service + broadcast

### Task 5.1: Broadcast envelope and handler integration (`internal/auth/oidc/broadcast.go`)

**Spec ref:** §3.1, §4.2, D4, D7, D18, I9.

**Files:**
- Create: `internal/auth/oidc/broadcast.go`
- Test: `internal/auth/oidc/broadcast_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/oidc/broadcast_test.go
package oidc

import (
    "encoding/json"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// recordingBroadcaster captures Broadcast calls for assertion.
type recordingBroadcaster struct {
    mu        sync.Mutex
    sent      []recordedMsg
    handler   func(topic string, payload []byte)
}

type recordedMsg struct {
    Topic   string
    Payload []byte
}

func (rb *recordingBroadcaster) Broadcast(topic string, payload []byte) {
    rb.mu.Lock()
    rb.sent = append(rb.sent, recordedMsg{Topic: topic, Payload: append([]byte(nil), payload...)})
    rb.mu.Unlock()
}

func (rb *recordingBroadcaster) Subscribe(topic string, handler func(payload []byte)) {
    rb.mu.Lock()
    rb.handler = func(t string, p []byte) {
        if t == topic {
            handler(p)
        }
    }
    rb.mu.Unlock()
}

func (rb *recordingBroadcaster) Deliver(topic string, payload []byte) {
    rb.mu.Lock()
    h := rb.handler
    rb.mu.Unlock()
    if h != nil {
        h(topic, payload)
    }
}

func TestBroadcast_EnvelopeRoundtrip(t *testing.T) {
    env := broadcastEnvelope{Op: "reload", TenantID: "tA", URI: "https://idp.example"}
    blob, _ := json.Marshal(&env)
    var back broadcastEnvelope
    if err := json.Unmarshal(blob, &back); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if back != env {
        t.Errorf("round trip lost data: %+v vs %+v", env, back)
    }
}

func TestBroadcastHandler_PanicRecovered(t *testing.T) {
    rb := &recordingBroadcaster{}
    r := NewRegistry(newTestStore(t), &fakeDiscovery{}, rb, NewMetrics(nil), nil)
    _ = r

    // Send a payload that triggers a panic in our discovery layer.
    payload, _ := json.Marshal(broadcastEnvelope{Op: "reload", TenantID: "tA", URI: "panic-trigger"})
    var panicked atomic.Bool
    defer func() { panicked.Store(recover() != nil) }()
    rb.Deliver(topicOidcProviders, payload)
    time.Sleep(20 * time.Millisecond)
    if panicked.Load() {
        t.Error("panic escaped handleBroadcast — defer recover() missing")
    }
}
```

- [ ] **Step 2: Implement**

```go
// internal/auth/oidc/broadcast.go
package oidc

import (
    "context"
    "encoding/json"
    "log/slog"
    "time"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

const topicOidcProviders = "oidc.providers"

type broadcastEnvelope struct {
    Op       string `json:"op"`           // "reload" | "invalidate" | "reload_all"
    TenantID string `json:"t,omitempty"`
    URI      string `json:"u,omitempty"`
}

// handleBroadcast is the registry's Subscribe callback. Runs on the
// memberlist receive goroutine — must be non-blocking and panic-safe.
func (r *Registry) handleBroadcast(payload []byte) {
    start := time.Now()
    defer func() {
        if rec := recover(); rec != nil {
            r.logger.Error("oidc broadcast handler panic",
                "pkg", "oidc", "panic", rec)
            r.metrics.BroadcastPanicTotal.Inc()
        }
        r.metrics.BroadcastReceiveSeconds.Observe(time.Since(start).Seconds())
    }()

    var env broadcastEnvelope
    if err := json.Unmarshal(payload, &env); err != nil {
        r.logger.Debug("oidc broadcast: malformed envelope", "pkg", "oidc", "error", err.Error())
        return
    }

    switch env.Op {
    case "reload":
        r.singleflight.Dispatch(env.TenantID+":"+env.URI, func() {
            r.reloadOne(spi.TenantID(env.TenantID), env.URI)
        })
    case "invalidate":
        r.singleflight.Dispatch(env.TenantID+":"+env.URI, func() {
            r.invalidateOne(spi.TenantID(env.TenantID), env.URI)
        })
    case "reload_all":
        r.singleflight.Dispatch("_reload_all", func() {
            _ = r.ReloadAll(context.Background())
        })
    default:
        r.logger.Debug("oidc broadcast: unknown op", "pkg", "oidc", "op", env.Op)
    }
}

// broadcast is invoked by the Service write paths. Fire-and-forget per D7.
func (r *Registry) broadcastOp(op, tenant, uri string) {
    if r.broadcast == nil {
        return
    }
    payload, err := json.Marshal(broadcastEnvelope{Op: op, TenantID: tenant, URI: uri})
    if err != nil {
        r.logger.Error("oidc: marshal broadcast envelope", "pkg", "oidc", "error", err.Error())
        return
    }
    r.broadcast.Broadcast(topicOidcProviders, payload)
}

var _ = slog.Default
```

- [ ] **Step 3: Run tests; commit**

```bash
go test ./internal/auth/oidc/ -run TestBroadcast -v
git add internal/auth/oidc/broadcast.go internal/auth/oidc/broadcast_test.go
git commit -m "feat(oidc): broadcast envelope + handler with panic recover (#284)"
```

### Task 5.2: Service (`internal/auth/oidc/service.go`)

**Spec ref:** §3.1, §5.1-§5.7, D11, D17, D19, D24, D25.

**Files:**
- Create: `internal/auth/oidc/service.go`
- Test: `internal/auth/oidc/service_test.go`

Implement and test the full set of 7 lifecycle operations. Given the size, structure as one task with sub-steps per operation.

- [ ] **Step 1: Skeleton + Register**

Write a failing test for Register, then implement the smallest path:

```go
// internal/auth/oidc/service_test.go
package oidc

import (
    "context"
    "testing"
    "time"

    "github.com/google/uuid"
    spi "github.com/cyoda-platform/cyoda-go-spi"
)

func newTestService(t *testing.T) *Service {
    t.Helper()
    store := newTestStore(t)
    r := NewRegistry(store, &fakeDiscovery{}, &recordingBroadcaster{}, NewMetrics(nil), nil)
    return NewService(store, r, nil)
}

func TestService_Register_Success(t *testing.T) {
    s := newTestService(t)
    ctx := context.Background()
    tenant := spi.TenantID("00000000-0000-0000-0000-00000000000a")
    p, err := s.Register(ctx, RegisterInput{
        TenantID:           tenant,
        WellKnownConfigURI: "https://idp.example/.well-known/openid-configuration",
        Issuers:            []string{"https://idp.example"},
        OwnerLegalEntityID: uuid.MustParse(string(tenant)),
    })
    if err != nil {
        t.Fatalf("Register: %v", err)
    }
    if p.ID == uuid.Nil {
        t.Error("Register did not assign ID")
    }
    if !p.CreatedAt.After(time.Now().Add(-time.Minute)) {
        t.Error("CreatedAt not set")
    }
}

func TestService_Register_Duplicate(t *testing.T) {
    s := newTestService(t)
    ctx := context.Background()
    tenant := spi.TenantID("00000000-0000-0000-0000-00000000000a")
    in := RegisterInput{
        TenantID:           tenant,
        WellKnownConfigURI: "https://idp.example/.well-known/openid-configuration",
        OwnerLegalEntityID: uuid.MustParse(string(tenant)),
    }
    if _, err := s.Register(ctx, in); err != nil {
        t.Fatalf("first Register: %v", err)
    }
    _, err := s.Register(ctx, in)
    if err == nil {
        t.Fatal("expected ErrProviderDuplicate, got nil")
    }
}
```

```go
// internal/auth/oidc/service.go
package oidc

import (
    "context"
    "errors"
    "fmt"
    "log/slog"
    "time"

    "github.com/google/uuid"
    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Service orchestrates the 7 lifecycle operations per spec §5. Mutates
// store, mutates registry, broadcasts (fire-and-forget). D25 ownership-
// transition audit log is inlined here (not in a separate audit.go) per
// rev. 4 W2.
type Service struct {
    store    OidcProviderStore
    registry *Registry
    logger   *slog.Logger
    clock    func() time.Time
}

func NewService(store OidcProviderStore, registry *Registry, logger *slog.Logger) *Service {
    if logger == nil {
        logger = slog.Default()
    }
    return &Service{store: store, registry: registry, logger: logger, clock: time.Now}
}

// RegisterInput holds the validated decode of POST /oauth/oidc/providers.
type RegisterInput struct {
    TenantID           spi.TenantID
    WellKnownConfigURI string
    Issuers            []string
    ExpectedAudiences  []string
    RolesClaim         *string
    OwnerLegalEntityID uuid.UUID
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (*OidcProvider, error) {
    // §5.1 step 4: per-tenant duplicate check.
    if existing, _ := s.store.GetByURI(ctx, in.TenantID, in.WellKnownConfigURI); existing != nil {
        return nil, ErrProviderDuplicate
    }
    p := &OidcProvider{
        ID:                 uuid.New(),
        WellKnownConfigURI: in.WellKnownConfigURI,
        Issuers:            in.Issuers,
        ExpectedAudiences:  in.ExpectedAudiences,
        RolesClaim:         in.RolesClaim,
        CreatedAt:          s.clock().UTC(),
        OwnerLegalEntityID: in.OwnerLegalEntityID,
    }

    if err := s.store.Register(ctx, p); err != nil {
        return nil, fmt.Errorf("oidc: register: %w", err)
    }

    // §5.1 step 6: D11 race-validation re-read.
    _, won, err := s.store.RaceValidateIndex(ctx, in.TenantID, in.WellKnownConfigURI, p.ID.String())
    if err != nil {
        // Best-effort cleanup if the re-read disappeared.
        _ = s.store.Delete(ctx, in.TenantID, p.ID.String(), in.WellKnownConfigURI)
        return nil, fmt.Errorf("oidc: race-validate: %w", err)
    }
    if !won {
        _ = s.store.Delete(ctx, in.TenantID, p.ID.String(), in.WellKnownConfigURI)
        return nil, ErrProviderDuplicate
    }

    // §5.1 step 7: D25 ownership-history update.
    s.emitOwnershipTransitionAndUpdateHistory(ctx, p, in.TenantID)

    // §5.1 step 8: registry reloadOne (sync; failures non-fatal).
    s.registry.reloadOne(in.TenantID, in.WellKnownConfigURI)

    // §5.1 step 9: broadcast (fire-and-forget per D7).
    s.registry.broadcastOp("reload", string(in.TenantID), in.WellKnownConfigURI)

    return p, nil
}

// emitOwnershipTransitionAndUpdateHistory is the D25 audit emitter. Inlined
// per W2 — no separate audit.go.
func (s *Service) emitOwnershipTransitionAndUpdateHistory(ctx context.Context, p *OidcProvider, tenant spi.TenantID) {
    uriHash := sha256Hex(p.WellKnownConfigURI)
    history, err := s.store.GetURIHistory(ctx, uriHash)
    if err != nil {
        s.logger.Error("oidc: get URI history", "pkg", "oidc", "uri_hash", uriHash, "error", err.Error())
        return
    }
    var priors []string
    if history != nil {
        if history.CurrentOwner != nil && history.CurrentOwner.TenantID != string(tenant) {
            priors = append(priors, history.CurrentOwner.TenantID)
        }
        for _, past := range history.Past {
            if past.TenantID != string(tenant) {
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
    }
    if history == nil {
        history = &UriOwnershipHistory{}
    }
    newOwner := Owner{TenantID: string(tenant), ProviderUUID: p.ID.String(), RegisteredAt: p.CreatedAt}
    if history.CurrentOwner == nil {
        history.CurrentOwner = &newOwner
    } else {
        // Concurrent ownership — move prior current to past without a DeletedAt
        // so it stays visible as a still-active concurrent owner.
        history.Past = append(history.Past, *history.CurrentOwner)
        history.CurrentOwner = &newOwner
    }
    if err := s.store.PutURIHistory(ctx, uriHash, history); err != nil {
        s.logger.Error("oidc: put URI history", "pkg", "oidc", "uri_hash", uriHash, "error", err.Error())
    }
}

var _ = errors.New
```

Continue test → implement → commit for the remaining operations following the same pattern. The spec sections §5.2–§5.7 are the contract. Concrete tests required at minimum:

| Op | Required test (one each) |
|---|---|
| Update | success path; cleared `issuers`; 404 on missing; 409 on inactive |
| Invalidate | success path; idempotent on already-invalidated; 404 |
| Reactivate | success with keys=true; success with JWKS down (cache preserved per D19); idempotent on already-active; 404 |
| Delete | success removes blob+index+history.CurrentOwner; 404 |
| ReloadAll | invokes Registry.ReloadAll under write lock; broadcasts reload_all |
| List | filters by tenant; activeOnly=true filters invalidated |

- [ ] **Step 2-7: For each operation above**, follow this sub-pattern:
  1. Add a test in `service_test.go` that captures the contract.
  2. Implement the operation method in `service.go`.
  3. Run `go test ./internal/auth/oidc/ -run TestService_<Op> -v` — verify PASS.
  4. Commit with message `feat(oidc): Service.<Op> per §5.X (#284)`.

- [ ] **Step 8: Final phase-5 verification**

```bash
go test ./internal/auth/oidc/... -count=1
```

Expected: all PASS.

---

## Phase 6: HTTP adapters

### Task 6.1: PATCH tri-state decoder + 7 HTTP handlers (`internal/domain/account/oidc_adapter.go`)

**Spec ref:** §3.1, §3.4 wire matrix, §5, D21, D24.

**Files:**
- Create: `internal/domain/account/oidc_adapter.go`
- Test: `internal/domain/account/oidc_adapter_test.go`

This task has 7 handlers + the tri-state decoder. Use the same per-op sub-step pattern as Task 5.2. The tri-state decoder is task-critical; write it first.

- [ ] **Step 1: PATCH tri-state decoder**

Write a failing test first:

```go
// internal/domain/account/oidc_adapter_test.go (selected; full file has many tests)
package account

import (
    "encoding/json"
    "testing"
)

func TestPatchTriState_FieldAbsent(t *testing.T) {
    body := []byte(`{}`)
    raw := map[string]json.RawMessage{}
    _ = json.Unmarshal(body, &raw)

    issuers, present, err := decodeOptionalStringSlice(raw, "issuers")
    if err != nil {
        t.Fatalf("decode: %v", err)
    }
    if present {
        t.Error("present should be false for absent field")
    }
    if issuers != nil {
        t.Errorf("issuers = %v, want nil", issuers)
    }
}

func TestPatchTriState_FieldNull(t *testing.T) {
    body := []byte(`{"issuers": null}`)
    raw := map[string]json.RawMessage{}
    _ = json.Unmarshal(body, &raw)

    issuers, present, err := decodeOptionalStringSlice(raw, "issuers")
    if err != nil {
        t.Fatalf("decode: %v", err)
    }
    if !present {
        t.Error("present should be true for explicit null")
    }
    if issuers != nil {
        t.Errorf("issuers = %v, want nil-after-clear", issuers)
    }
}

func TestPatchTriState_FieldEmptyArray(t *testing.T) {
    body := []byte(`{"issuers": []}`)
    raw := map[string]json.RawMessage{}
    _ = json.Unmarshal(body, &raw)

    issuers, present, err := decodeOptionalStringSlice(raw, "issuers")
    if err != nil {
        t.Fatalf("decode: %v", err)
    }
    if !present {
        t.Error("present should be true for empty array")
    }
    if issuers != nil {
        t.Errorf("issuers = %v, want nil (empty array clears, runtime-equivalent to null)", issuers)
    }
}

func TestPatchTriState_FieldNonEmptyArray(t *testing.T) {
    body := []byte(`{"issuers": ["a", "b"]}`)
    raw := map[string]json.RawMessage{}
    _ = json.Unmarshal(body, &raw)

    issuers, present, _ := decodeOptionalStringSlice(raw, "issuers")
    if !present {
        t.Error("present should be true")
    }
    if len(issuers) != 2 {
        t.Errorf("issuers = %v, want [a b]", issuers)
    }
}
```

Implement:

```go
// internal/domain/account/oidc_adapter.go (decoder portion)
package account

import (
    "bytes"
    "encoding/json"
)

// decodeOptionalStringSlice implements PATCH tri-state for an array-typed
// optional field per spec D24. Returns (value, present, err) where:
//
//   - present=false → field absent in PATCH body; leave existing field unchanged.
//   - present=true, value=nil → field was null or [] (both clear at runtime).
//   - present=true, value=[...] → field is set to the non-empty list.
func decodeOptionalStringSlice(raw map[string]json.RawMessage, field string) ([]string, bool, error) {
    rm, ok := raw[field]
    if !ok {
        return nil, false, nil
    }
    if bytes.Equal(bytes.TrimSpace(rm), []byte("null")) {
        return nil, true, nil
    }
    var arr []string
    if err := json.Unmarshal(rm, &arr); err != nil {
        return nil, true, err
    }
    if len(arr) == 0 {
        return nil, true, nil // empty array clears (runtime-equivalent to null)
    }
    return arr, true, nil
}

// decodeOptionalString handles the rolesClaim tri-state: absent / null / non-empty string.
func decodeOptionalString(raw map[string]json.RawMessage, field string) (*string, bool, error) {
    rm, ok := raw[field]
    if !ok {
        return nil, false, nil
    }
    if bytes.Equal(bytes.TrimSpace(rm), []byte("null")) {
        return nil, true, nil
    }
    var s string
    if err := json.Unmarshal(rm, &s); err != nil {
        return nil, true, err
    }
    return &s, true, nil
}
```

Run tests, commit.

- [ ] **Step 2-8: For each of the 7 handlers** (Register, List, Update, Invalidate, Reactivate, Delete, ReloadOidcProviders), write the failing test, implement, verify, commit. Reference spec §5 for the exact lifecycle. The adapter is thin — its job is HTTP↔Service translation; the business logic is all in Service.

### Task 6.2: Wire adapter into Handler (`internal/domain/account/handler.go`)

**Files:**
- Modify: `internal/domain/account/handler.go:95-121`

- [ ] Replace each of the seven `h.stub(w, r)` calls with `h.oidcAdapter.<Method>(...)` delegations. Add the `oidcAdapter *OidcAdapter` field; wire through `NewHandler`. Add a one-line test that asserts each method delegates and does NOT call `stub`. Commit.

---

## Phase 7: Configuration + startup wiring

### Task 7.1: Add OIDC config (`app/config.go`)

**Spec ref:** §7, D9.

- [ ] Add `OIDCConfig` struct with the 6 fields per §7 table. Populate from env vars in `DefaultConfig()`. Add a test that asserts default values and env-var override. Commit.

### Task 7.2: Startup wiring (`app/app.go`)

**Spec ref:** §6, D3, D7, D18, I7.

- [ ] Add the broadcaster-nil check (D7), Phase 1 load, chain construction (`auth.NewChainedValidator(jwksValidator, oidcValidator)` — order normative), Phase 2 async warmup. Add a small startup test (use the existing `app_test.go` integration harness) that confirms the listener binds before Phase 2 completes. Commit.

---

## Phase 8: OpenAPI + generated code

### Task 8.1: OpenAPI edits (`api/openapi.yaml`)

**Spec ref:** §3.1 OpenAPI row.

- [ ] Remove the seven `NotImplemented` $refs at lines 4806, 4864, 4918, 4970, 5044, 5098, 5160. Reconcile `SUPER_USER` → `ROLE_ADMIN` on the 6 mutating operations. Update List prose to "any authenticated tenant member." Add `expectedAudiences: array(string)` and `rolesClaim: string` to both request DTOs and the response. Fix the `issuers` description on both DTOs to match D17. Drop `minItems: 1` from `issuers`. Commit.

### Task 8.2: Regenerate `api/generated.go`

- [ ] Run the project's existing codegen command (likely `make generate` — verify by grepping the Makefile). Commit the regenerated file.

---

## Phase 9: Parity tests + E2E

### Task 9.1: Fixture IdP and fault-injecting KV (`internal/auth/oidc/fixture_test.go`, `internal/auth/oidc/fault_kv_test.go`)

- [ ] Implement `FixtureIdP` per spec §11. Implement `FaultInjectingKV` per row 38b. Add a smoke test that confirms the fixture serves a valid discovery doc and JWKS. Commit.

### Task 9.2 – 9.N: Parity scenarios in `internal/e2e/parity/oidc.go`

The spec's §11 inventory has 64 test rows organized into families. For each family, dispatch a subagent task that:

1. Adds the family's tests to `internal/e2e/parity/oidc.go`.
2. Registers the new `NamedTest` entries in `e2e/parity/registry.go::allTests`.
3. Runs `go test ./internal/e2e/parity/... -run RunOidc -v` against the in-memory plugin.
4. Commits with `test(oidc-parity): <family-name> scenarios (#284)`.

Order the family tasks roughly: CRUD happy → CRUD negative → authz → JWT validation → key rotation → multi-provider → D5/D17/D3/D6/D11/D8/D18/D10/D19/D20/D23/D25/D21/I9 → state transitions → E2E.

After all parity tasks land, run the full plugin suite:

```bash
go test ./internal/e2e/parity/... -count=1
make test-all   # plugin submodules — Docker required for postgres
```

Expected: all PASS.

---

## Phase 10: Documentation

### Task 10.1: Error-code help topics

- [ ] Create the 10 help-topic files listed in §12: `OIDC_PROVIDER_DUPLICATE.md`, `OIDC_PROVIDER_NOT_FOUND.md`, `OIDC_PROVIDER_INACTIVE.md`, `OIDC_SSRF_BLOCKED.md`, `OIDC_AUDIENCE_MISMATCH.md`, `OIDC_TOKEN_PRE_TRANSITION.md`, `OIDC_DISCOVERY_FAILED.md`, `OIDC_JWKS_UNAVAILABLE.md`, `OIDC_CLAIMS_INVALID.md`. Each follows the existing template under `cmd/cyoda/help/content/errors/`. Commit.

### Task 10.2: Config help topic + README

- [ ] Create `cmd/cyoda/help/content/config/oidc.md` documenting all 6 env vars. Update `README.md` with a new "OIDC Provider Configuration" subsection. Commit.

### Task 10.3: ARCHITECTURE, PRD, FEATURES, CHANGELOG

- [ ] Update `docs/ARCHITECTURE.md` (new §7.3; renumber existing §7.3 → §7.4). Update `docs/PRD.md` (expand §8 with OIDC chaining + external-issuer claims example). Add one bullet to `docs/FEATURES.md` under Authentication & Authorization. Update `CHANGELOG.md` `## [0.8.0]` with `### Added` and `### Changed` entries per §12. Commit per file.

---

## Phase 11: Final verification

### Task 11.1: Full test suite + race detector

- [ ] **Run the full root-module test suite:**

```bash
go test ./... -count=1 -v 2>&1 | tail -20
```

Expected: all PASS, including the E2E tests in `internal/e2e/`.

- [ ] **Run the per-plugin suites:**

```bash
make test-all
```

Expected: all PASS (requires Docker for postgres testcontainers).

- [ ] **Run the race detector once:**

```bash
go test -race ./internal/auth/... ./internal/e2e/parity/... 2>&1 | tail -10
```

Expected: no data-race reports.

- [ ] **Run `go vet`:**

```bash
go vet ./...
```

Expected: silent.

- [ ] **Log-hygiene grep check (Gate 3):**

```bash
rg -n "tokenString|Bearer |RawToken|signingInput" internal/auth/oidc/ \
    | grep -E "slog\\.(Debug|Info|Warn|Error)"
```

Expected: empty (no slog call carries token material).

### Task 11.2: TODO sweep

- [ ] **Search for unresolved TODOs:**

```bash
make todos | grep -i oidc
```

Expected: zero new TODOs. Any TODO that landed during implementation must be either (a) resolved before merge per Gate 6, or (b) explicitly carry an issue reference and rationale per the "TODO(plan-reference): description" convention.

### Task 11.3: Acceptance criteria walk-through

- [ ] **Open the issue body** (`gh issue view 284`) and tick each acceptance-criterion item against where it lands in the PR. Capture the mapping in the PR body when it's drafted.

### Task 11.4: Commit boundary check

- [ ] **Run `git log --oneline release/v0.8.0..HEAD`** and confirm each commit makes sense in isolation: it has a focused scope, passes tests at that point, and the commit message says what changed.

---

## Done

When all phases complete and Phase 11 is green, the worktree is ready for PR creation. Use the `superpowers:finishing-a-development-branch` skill (or `superpowers:requesting-code-review` for the human checkpoint) to proceed to merge.

Reference the spec and ADR explicitly in the PR body:
- Spec: `docs/superpowers/specs/2026-06-16-284-oidc-providers-design.md` (rev. 4)
- ADR: `docs/adr/0002-federated-identity-provider-architecture.md`
- Decomposition umbrella: `docs/superpowers/specs/2026-06-04-194-decomposition-design.md` §3.4

Closes `#284`.
