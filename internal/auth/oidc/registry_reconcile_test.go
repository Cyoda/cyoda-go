package oidc

import (
	"context"
	"crypto/rsa"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// stubKeySource is a minimal auth.KeySource that always reports the key as
// not found. Used where the test only needs a non-nil source to be carried
// over by ReloadAll — no key resolution happens through it.
type stubKeySource struct{}

func (stubKeySource) GetKey(kid string) (*rsa.PublicKey, error) { return nil, auth.ErrKeyNotFound }

// registryOverStore builds a Registry over a KV-backed provider store with
// a fakeDiscovery, plus the store handle for direct mutations.
func registryOverStore(t *testing.T) (*Registry, OidcProviderStore, *fakeDiscovery) {
	t.Helper()
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{AllowPrivateNetworks: true})
	return r, store, disc
}

func reconcileTestProvider(t *testing.T, uri string) *OidcProvider {
	t.Helper()
	return &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		CreatedAt:          time.Now().UTC(),
		OwnerLegalEntityID: uuid.New(),
	}
}

// A provider deleted straight in the store (no broadcast) disappears from
// the registry after ReloadAll; a surviving provider's installed source is
// carried over untouched.
func TestReloadAll_DropsDeletedProvider_CarriesSurvivorSource(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()

	pDoomed := reconcileTestProvider(t, "https://doomed.example/.well-known/openid-configuration")
	pKeep := reconcileTestProvider(t, "https://keep.example/.well-known/openid-configuration")
	if err := store.Register(ctx, pDoomed); err != nil {
		t.Fatalf("Register doomed: %v", err)
	}
	if err := store.Register(ctx, pKeep); err != nil {
		t.Fatalf("Register keep: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	keepTenant := spi.TenantID(pKeep.OwnerLegalEntityID.String())
	// Install a sentinel source for the survivor so carry-over is observable.
	r.installForTest(pKeep, stubKeySource{}, &DiscoveryDoc{Issuer: "https://keep.example"})

	if err := store.Delete(ctx, spi.TenantID(pDoomed.OwnerLegalEntityID.String()), pDoomed.ID.String(), pDoomed.WellKnownConfigURI); err != nil {
		t.Fatalf("store Delete: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll after delete: %v", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, still := r.providers[spi.TenantID(pDoomed.OwnerLegalEntityID.String())][pDoomed.WellKnownConfigURI]; still {
		t.Fatal("deleted provider survived ReloadAll")
	}
	src := r.sources[keepTenant][pKeep.WellKnownConfigURI]
	if src == nil || src.discoveryDoc == nil {
		t.Fatal("survivor's warm source was not carried over")
	}
}

// T9: identical snapshot ⇒ swap skipped: kidIndex survives, sources kept,
// orphaned sources pruned, success accounting still updated.
func TestReloadAll_SkipSwapOnIdenticalSnapshot(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()
	p := reconcileTestProvider(t, "https://stable.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())

	// Seed kidIndex and an orphan source.
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.kidIndex["kid-1"] = []providerRef{{tenant: tenant, uri: p.WellKnownConfigURI}}
		if r.sources["ghost-tenant"] == nil {
			r.sources["ghost-tenant"] = map[string]*providerSource{}
		}
		r.sources["ghost-tenant"]["https://ghost.example"] = &providerSource{}
	}()

	genBefore := r.mapGen.Load()
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("quiet-tick ReloadAll: %v", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.kidIndex["kid-1"]) != 1 {
		t.Fatal("skip-swap tick reset kidIndex")
	}
	if _, still := r.sources["ghost-tenant"]["https://ghost.example"]; still {
		t.Fatal("orphaned source not pruned on skip-swap tick")
	}
	if r.mapGen.Load() != genBefore {
		t.Fatal("skip-swap must not bump the generation (no map change)")
	}
	if r.lastReconcileNanos.Load() == 0 {
		t.Fatal("skip-swap tick must still count as a successful reconcile")
	}
}

// T10: a mutation racing ReloadAll's build window is not clobbered.
func TestReloadAll_GenerationGuard(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()
	p := reconcileTestProvider(t, "https://raced.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())

	// loadAllHook fires inside ReloadAll after store.LoadAll returns (see
	// implementation) — simulate a dropped-invalidate peer delete landing
	// locally mid-build.
	fired := false
	r.loadAllHook = func() {
		if fired {
			return
		}
		fired = true
		if err := store.Delete(ctx, tenant, p.ID.String(), p.WellKnownConfigURI); err != nil {
			t.Errorf("hook Delete: %v", err)
		}
		r.invalidateOne(tenant, p.WellKnownConfigURI)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll with racing mutation: %v", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, resurrected := r.providers[tenant][p.WellKnownConfigURI]; resurrected {
		t.Fatal("stale snapshot resurrected a provider deleted mid-build")
	}
}

// T10: overlapping ReloadAll executions serialize; nothing resurrects.
func TestReloadAll_ConcurrentCallsSerialized(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()
	p := reconcileTestProvider(t, "https://conc.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.ReloadAll(ctx)
		}()
	}
	wg.Wait()
	r.mu.RLock()
	defer r.mu.RUnlock()
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	if _, ok := r.providers[tenant][p.WellKnownConfigURI]; !ok {
		t.Fatal("provider lost under concurrent ReloadAll")
	}
}

// T8: the loop converges a store-level delete with no broadcast at all.
func TestRegistry_ReconcileLoop_ConvergesStoreDelete(t *testing.T) {
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    20 * time.Millisecond,
	})
	ctx := context.Background()
	p := reconcileTestProvider(t, "https://loop.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !r.StartReconcileLoop(loopCtx) {
		t.Fatal("StartReconcileLoop returned false on first call")
	}
	if r.StartReconcileLoop(loopCtx) {
		t.Fatal("second StartReconcileLoop must return false")
	}

	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	if err := store.Delete(ctx, tenant, p.ID.String(), p.WellKnownConfigURI); err != nil {
		t.Fatalf("store Delete: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gone := func() bool {
			r.mu.RLock()
			defer r.mu.RUnlock()
			_, ok := r.providers[tenant][p.WellKnownConfigURI]
			return !ok
		}()
		if gone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reconcile loop never dropped the store-deleted provider")
}

// T11: composition — a provider registered on a peer (no broadcast) becomes
// resolvable via reconcile discovery + warmup-retry warm.
func TestRegistry_ReconcilePlusWarmupRetry_Composition(t *testing.T) {
	idp := NewFixtureIdP(t)
	store := newTestStore(t)
	disc := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    20 * time.Millisecond,
		WarmupRetryInterval:  20 * time.Millisecond,
	})
	ctx := context.Background()
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !r.StartReconcileLoop(loopCtx) {
		t.Fatal("StartReconcileLoop")
	}
	if !r.StartWarmupRetryLoop(loopCtx) {
		t.Fatal("StartWarmupRetryLoop")
	}

	// "Peer" registers straight into the store — this node hears nothing.
	p := reconcileTestProvider(t, idp.Server.URL+"/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	kid := "default"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := r.ResolveKey(kid, idp.Issuer, ""); err == nil && res != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("peer-registered provider never became resolvable via reconcile+warmup composition")
}

// T12: ResolveKey fails closed past the staleness bound and recovers.
func TestRegistry_ResolveKey_StalenessBreaker(t *testing.T) {
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    20 * time.Millisecond,
	})
	ctx := context.Background()
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	// Not started → breaker inert even with an ancient lastReconcile.
	r.lastReconcileNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	if _, err := r.ResolveKey("any-kid", "https://iss.example", ""); errors.Is(err, ErrRegistryStale) {
		t.Fatal("breaker tripped before the reconcile loop ever started")
	}

	// Started + stale → ErrRegistryStale.
	loopCtx, cancel := context.WithCancel(ctx)
	cancel() // loop exits immediately; started-flag remains set
	r.StartReconcileLoop(loopCtx)
	r.lastReconcileNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	if _, err := r.ResolveKey("any-kid", "https://iss.example", ""); !errors.Is(err, ErrRegistryStale) {
		t.Fatalf("want ErrRegistryStale past the bound, got %v", err)
	}

	// Recovery: a successful reconcile resets the clock.
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("recovery ReloadAll: %v", err)
	}
	if _, err := r.ResolveKey("any-kid", "https://iss.example", ""); errors.Is(err, ErrRegistryStale) {
		t.Fatal("breaker still tripped after successful reconcile")
	}
}

// blockingStore is an OidcProviderStore stub whose LoadAll blocks until the
// ctx passed to it is done, then returns ctx.Err(). Every other method
// panics — this stub only ever drives ReloadAll's LoadAll call. Used to
// prove a hung store call cannot hold reconcileMu (and therefore block
// every serialized caller, including the periodic loop) indefinitely.
type blockingStore struct{}

func (blockingStore) Register(context.Context, *OidcProvider) error { panic("not implemented") }
func (blockingStore) Get(context.Context, spi.TenantID, string) (*OidcProvider, error) {
	panic("not implemented")
}
func (blockingStore) GetByURI(context.Context, spi.TenantID, string) (*OidcProvider, error) {
	panic("not implemented")
}
func (blockingStore) Update(context.Context, *OidcProvider) error { panic("not implemented") }
func (blockingStore) Delete(context.Context, spi.TenantID, string, string) error {
	panic("not implemented")
}
func (blockingStore) ListByTenant(context.Context, spi.TenantID, bool) ([]*OidcProvider, error) {
	panic("not implemented")
}
func (blockingStore) LoadAll(ctx context.Context) ([]*OidcProvider, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingStore) GetURIHistory(context.Context, string) (*UriOwnershipHistory, error) {
	panic("not implemented")
}
func (blockingStore) PutURIHistory(context.Context, string, *UriOwnershipHistory) error {
	panic("not implemented")
}
func (blockingStore) RaceValidateIndex(context.Context, spi.TenantID, string, string) (string, bool, error) {
	panic("not implemented")
}

var _ OidcProviderStore = blockingStore{}

// Finding 1: two production callers (the reload_all broadcast handler, the
// admin reload endpoint) enter ReloadAll with a deadline-free ctx. Without a
// per-attempt bound, a hung store.LoadAll would hold reconcileMu forever,
// wedging the periodic loop behind it. ReloadAll must bound each attempt
// itself and return (with an error) instead of hanging.
func TestReloadAll_BoundedByPerAttemptTimeout_HungStore(t *testing.T) {
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(blockingStore{}, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    50 * time.Millisecond,
	})

	done := make(chan error, 1)
	go func() {
		done <- r.ReloadAll(context.Background()) // deadline-free, mirrors the production callers
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from a per-attempt-timeout-bounded ReloadAll against a hung store")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReloadAll did not return within the bound; a hung store call held reconcileMu")
	}
}

// cancelSignalStore is an OidcProviderStore stub whose LoadAll blocks until
// ctx is done, signalling entered (closed exactly once) the moment it starts
// waiting so a test can deterministically cancel the parent ctx while
// LoadAll is in flight. Every other method panics.
type cancelSignalStore struct{ entered chan struct{} }

func (cancelSignalStore) Register(context.Context, *OidcProvider) error { panic("not implemented") }
func (cancelSignalStore) Get(context.Context, spi.TenantID, string) (*OidcProvider, error) {
	panic("not implemented")
}
func (cancelSignalStore) GetByURI(context.Context, spi.TenantID, string) (*OidcProvider, error) {
	panic("not implemented")
}
func (cancelSignalStore) Update(context.Context, *OidcProvider) error { panic("not implemented") }
func (cancelSignalStore) Delete(context.Context, spi.TenantID, string, string) error {
	panic("not implemented")
}
func (cancelSignalStore) ListByTenant(context.Context, spi.TenantID, bool) ([]*OidcProvider, error) {
	panic("not implemented")
}
func (s cancelSignalStore) LoadAll(ctx context.Context) ([]*OidcProvider, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (cancelSignalStore) GetURIHistory(context.Context, string) (*UriOwnershipHistory, error) {
	panic("not implemented")
}
func (cancelSignalStore) PutURIHistory(context.Context, string, *UriOwnershipHistory) error {
	panic("not implemented")
}
func (cancelSignalStore) RaceValidateIndex(context.Context, spi.TenantID, string, string) (string, bool, error) {
	panic("not implemented")
}

var _ OidcProviderStore = cancelSignalStore{}

// Finding 2: a client disconnect on the admin reload endpoint cancels the
// request ctx mid-LoadAll. Because Finding 1 derives each attempt's timeout
// from that same ctx, the store call surfaces context.Canceled (the parent
// lost its race with the timer), not context.DeadlineExceeded. This is the
// caller aborting, not a KV failure, so it must not pollute
// consecutiveFailures / ERROR-escalation accounting.
func TestReloadAll_ClientDisconnect_DoesNotPolluteFailureAccounting(t *testing.T) {
	store := cancelSignalStore{entered: make(chan struct{})}
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    time.Second, // long enough that the timer can't race the explicit cancel
	})

	parentCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.ReloadAll(parentCtx)
	}()

	<-store.entered
	cancel() // simulates the HTTP client disconnecting mid-request

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReloadAll did not return after parent ctx cancellation")
	}

	if got := r.consecutiveFailures.Load(); got != 0 {
		t.Fatalf("client disconnect must not pollute failure accounting: consecutiveFailures=%d", got)
	}
}

// Finding 4 (coverage gap): no test previously drove ReloadAll's retry
// budget to exhaustion. Mutating the provider map on every loadAllHook
// invocation (fired after every attempt's LoadAll, before the swap's
// generation check) guarantees the generation guard trips on all
// maxReloadAttempts attempts.
func TestReloadAll_RetryBudgetExhausted_ContentionUnderConcurrentMutation(t *testing.T) {
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	metrics := &recordingMetrics{}
	r := NewRegistry(store, disc, nil, metrics, nil, RegistryConfig{AllowPrivateNetworks: true})
	ctx := context.Background()
	p := reconcileTestProvider(t, "https://churn.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("warm-up ReloadAll: %v", err)
	}
	lastBefore := r.lastReconcileNanos.Load()
	stalenessCallsBefore := atomic.LoadInt64(&metrics.reconcileStalenessCalls)
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())

	// Mutate the provider map on every attempt so the generation guard never
	// sees a stable snapshot; ReloadAll exhausts its retry budget.
	r.loadAllHook = func() {
		r.invalidateOne(tenant, p.WellKnownConfigURI)
		r.addToProviderMap(p)
	}
	err := r.ReloadAll(ctx)
	if !errors.Is(err, errReloadContention) {
		t.Fatalf("want errReloadContention, got %v", err)
	}
	if r.lastReconcileNanos.Load() != lastBefore {
		t.Fatal("lastReconcileNanos must not advance on contention exhaustion")
	}
	if got := r.consecutiveFailures.Load(); got != 0 {
		t.Fatalf("contention exhaustion is not a KV failure: consecutiveFailures=%d", got)
	}

	// M1 fix: the staleness gauge must still be recorded on contention
	// exhaustion so alerting on age > 10x interval is never blinded by a
	// gauge that reads its last (typically zero) value under sustained
	// mutation churn. The consecutiveFailures gauge must NOT be touched —
	// contention is not a KV failure.
	if atomic.LoadInt64(&metrics.reconcileStalenessCalls) == stalenessCallsBefore {
		t.Fatal("contention exhaustion must record the staleness gauge so alerting is not blinded")
	}
	if atomic.LoadInt64(&metrics.reconcileStalenessMillis) < 0 {
		t.Fatalf("staleness gauge recorded a negative value: %dms", metrics.reconcileStalenessMillis)
	}
	if metrics.reconcileFailures != 0 {
		t.Fatalf("contention exhaustion must not touch the consecutiveFailures gauge (not a KV failure): %d", metrics.reconcileFailures)
	}

	// A subsequent clean ReloadAll (no racing mutation) succeeds.
	r.loadAllHook = nil
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("clean ReloadAll after contention: %v", err)
	}
}
