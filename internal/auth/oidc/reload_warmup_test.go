package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/google/uuid"
)

// Tests for ReloadAll key-source preservation and force-warm semantics.
//
// Contract under test: POST /oauth/oidc/providers/reload must force-warm the
// JWKS of every active provider it reloads, and must never leave the registry
// with fewer warm key sources than before the call. A reload whose discovery
// fetch fails keeps serving the previously cached keys.

// flakyDiscovery is a mutable, concurrency-safe Discovery fake: err can be
// flipped at runtime to simulate an IdP that is down and later comes up.
type flakyDiscovery struct {
	mu      sync.Mutex
	docs    map[string]*DiscoveryDoc
	err     error
	fetches map[string]int
}

func (f *flakyDiscovery) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
	// Respect ctx like the real HTTPDiscovery does.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetches == nil {
		f.fetches = map[string]int{}
	}
	f.fetches[uri]++
	if f.err != nil {
		return nil, f.err
	}
	if d, ok := f.docs[uri]; ok {
		return d, nil
	}
	return nil, ErrDiscoveryFailed
}

func (f *flakyDiscovery) set(err error, docs map[string]*DiscoveryDoc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	if docs != nil {
		f.docs = docs
	}
}

func (f *flakyDiscovery) fetchCount(uri string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches[uri]
}

// plantProvider persists p in the registry's store so ReloadAll finds it.
func plantProvider(t *testing.T, r *Registry, p *OidcProvider) {
	t.Helper()
	if err := r.store.Register(context.Background(), p); err != nil {
		t.Fatalf("store.Register: %v", err)
	}
}

func testProvider(issuer, uri string) *OidcProvider {
	return &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		Issuers:            []string{issuer},
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: uuid.New(),
	}
}

func TestRegistry_ReloadAll_PreservesInstalledSources(t *testing.T) {
	// A provider whose key source is already warm must keep resolving after
	// ReloadAll rebuilds the provider map, even though no warm-up runs.
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	p := testProvider("https://idp.example", "https://idp.example/.well-known/openid-configuration")
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})
	plantProvider(t, r, p)

	if _, err := r.ResolveKey("k1", "https://idp.example", ""); err != nil {
		t.Fatalf("baseline ResolveKey: %v", err)
	}

	if err := r.ReloadAll(context.Background()); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	res, err := r.ResolveKey("k1", "https://idp.example", "")
	if err != nil {
		t.Fatalf("ResolveKey after ReloadAll: %v (installed source was not preserved)", err)
	}
	if res.Provider.ID != p.ID {
		t.Errorf("resolved provider ID = %s, want %s", res.Provider.ID, p.ID)
	}
}

func TestRegistry_ReloadAll_DropsSourcesForRemovedProviders(t *testing.T) {
	// A provider deleted from the store must not survive ReloadAll via a
	// carried-over source — carry-over applies only to coordinates that the
	// fresh store snapshot still contains.
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	p := testProvider("https://gone.example", "https://gone.example/.well-known/openid-configuration")
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://gone.example", JWKSURI: "https://gone.example/jwks"})
	// NOT planted in the store: the store snapshot is empty.

	if err := r.ReloadAll(context.Background()); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	if _, err := r.ResolveKey("k1", "https://gone.example", ""); !errors.Is(err, auth.ErrUnknownKID) {
		t.Fatalf("ResolveKey after ReloadAll = %v, want ErrUnknownKID for removed provider", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for tenant, byURI := range r.sources {
		if len(byURI) != 0 {
			t.Errorf("sources for tenant %s not dropped: %d entries", tenant, len(byURI))
		}
	}
}

func TestService_ReloadAll_WarmsColdProviders(t *testing.T) {
	// A provider present in the store but never warmed on this node (e.g. the
	// post-registration warm-up race) must be resolvable after the reload
	// endpoint runs: Service.ReloadAll force-warms what it loads.
	idp := NewFixtureIdP(t)
	uri := idp.Issuer + "/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{docs: map[string]*DiscoveryDoc{
		uri: {Issuer: idp.Issuer, JWKSURI: idp.JWKSURI},
	}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})
	s := NewService(store, r, nil)

	p := testProvider(idp.Issuer, uri)
	plantProvider(t, r, p)

	if err := s.ReloadAll(context.Background()); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	if _, err := r.ResolveKey("default", idp.Issuer, ""); err != nil {
		t.Fatalf("ResolveKey after Service.ReloadAll: %v (cold provider was not warmed)", err)
	}
}

func TestService_ReloadAll_KeepsCachedKeysWhenIdPUnreachable(t *testing.T) {
	// Reload with the IdP down must not destroy the working key source: the
	// warm-up fails, the carried-over source keeps serving.
	store := newTestStore(t)
	disc := &flakyDiscovery{err: ErrDiscoveryFailed}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})
	s := NewService(store, r, nil)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	p := testProvider("https://idp.example", "https://idp.example/.well-known/openid-configuration")
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})
	plantProvider(t, r, p)

	if err := s.ReloadAll(context.Background()); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	if _, err := r.ResolveKey("k1", "https://idp.example", ""); err != nil {
		t.Fatalf("ResolveKey after ReloadAll with IdP down: %v (cached keys were destroyed)", err)
	}
}

func TestRegistry_WarmJWKS_SkipsInactiveProviders(t *testing.T) {
	// The warm-up must not fetch discovery/JWKS for invalidated providers:
	// an admin has explicitly distrusted those endpoints, and reactivation
	// re-warms explicitly.
	inactiveURI := "https://inactive.example/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{docs: map[string]*DiscoveryDoc{
		inactiveURI: {Issuer: "https://inactive.example", JWKSURI: "https://inactive.example/jwks"},
	}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})

	inactive := testProvider("https://inactive.example", inactiveURI)
	invalidatedAt := time.Now()
	inactive.InvalidatedAt = &invalidatedAt
	plantProvider(t, r, inactive)

	ctx := context.Background()
	if err := r.LoadProvidersFromKV(ctx); err != nil {
		t.Fatalf("LoadProvidersFromKV: %v", err)
	}
	r.WarmJWKS(ctx)

	if n := disc.fetchCount(inactiveURI); n != 0 {
		t.Errorf("WarmJWKS fetched an invalidated provider %d times, want 0", n)
	}
}

func TestRegistry_ReloadAllAndWarm_DetachedFromCallerCancellation(t *testing.T) {
	// The force-warm is a cluster-visible operation: an HTTP client
	// disconnecting mid-request must not abort it half way. The warm phase
	// runs detached from the caller's cancellation.
	idp := NewFixtureIdP(t)
	uri := idp.Issuer + "/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{docs: map[string]*DiscoveryDoc{
		uri: {Issuer: idp.Issuer, JWKSURI: idp.JWKSURI},
	}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})

	p := testProvider(idp.Issuer, uri)
	plantProvider(t, r, p)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // caller is already gone before the warm starts

	if err := r.ReloadAllAndWarm(cancelled); err != nil {
		t.Fatalf("ReloadAllAndWarm: %v", err)
	}

	if _, err := r.ResolveKey("default", idp.Issuer, ""); err != nil {
		t.Fatalf("ResolveKey after cancelled-caller reload: %v (warm was aborted by caller cancellation)", err)
	}
}

func TestRegistry_StartWarmupRetryLoop_SecondCallIsNoop(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if started := r.StartWarmupRetryLoop(ctx); !started {
		t.Error("first StartWarmupRetryLoop call = false, want true")
	}
	if started := r.StartWarmupRetryLoop(ctx); started {
		t.Error("second StartWarmupRetryLoop call = true, want false (loop already running)")
	}
}

func TestRegistry_WarmupRetryLoop_StopsOnContextCancel(t *testing.T) {
	uri := "https://slow-idp.example/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{err: ErrDiscoveryFailed} // stays down: every tick fetches
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true, WarmupRetryInterval: 10 * time.Millisecond})

	p := testProvider("https://slow-idp.example", uri)
	plantProvider(t, r, p)
	ctx := context.Background()
	if err := r.LoadProvidersFromKV(ctx); err != nil {
		t.Fatalf("LoadProvidersFromKV: %v", err)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	r.StartWarmupRetryLoop(loopCtx)

	// Let it tick a few times, then cancel and let any in-flight tick drain.
	time.Sleep(60 * time.Millisecond)
	cancel()
	time.Sleep(60 * time.Millisecond)

	before := disc.fetchCount(uri)
	if before == 0 {
		t.Fatal("retry loop never ticked before cancellation")
	}
	time.Sleep(200 * time.Millisecond)
	if after := disc.fetchCount(uri); after != before {
		t.Errorf("retry loop still fetching after ctx cancel: %d -> %d", before, after)
	}
}

func TestRegistry_ReloadOne_PinMismatchDropsCarriedSource(t *testing.T) {
	// An issuer-pin mismatch is an affirmative conflict, not a transient
	// failure: the IdP has contradicted the binding the carried keys were
	// fetched under. The carried source must be dropped — fail closed —
	// not kept serving like it is on an unreachable IdP.
	uri := "https://pinned.example/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{docs: map[string]*DiscoveryDoc{
		uri: {Issuer: "https://rogue.example", JWKSURI: "https://pinned.example/jwks"},
	}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	p := testProvider("https://pinned.example", uri)
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://pinned.example", JWKSURI: "https://pinned.example/jwks"})

	if _, err := r.ResolveKey("k1", "https://pinned.example", ""); err != nil {
		t.Fatalf("baseline ResolveKey: %v", err)
	}

	r.reloadOne(context.Background(), spi.TenantID(p.OwnerLegalEntityID.String()), uri)

	if _, err := r.ResolveKey("k1", "https://pinned.example", ""); err == nil {
		t.Fatal("ResolveKey still succeeds after issuer-pin mismatch (carried source not dropped)")
	}
}

func TestRegistry_ReloadAllAndWarm_ConcurrentCallsSerialize(t *testing.T) {
	// Concurrent force-warm reloads (e.g. an admin looping the endpoint)
	// must serialize into one fetch pool at a time, not multiply outbound
	// traffic to every tenant's IdP.
	uri := "https://slow.example/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &slowCountingDiscovery{delay: 50 * time.Millisecond}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})

	p := testProvider("https://slow.example", uri)
	plantProvider(t, r, p)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.ReloadAllAndWarm(context.Background())
		}()
	}
	wg.Wait()

	if peak := disc.peakConcurrent(); peak > 1 {
		t.Errorf("peak concurrent discovery fetches = %d, want 1 (warm pools not serialized)", peak)
	}
}

// slowCountingDiscovery fails every fetch after a delay while tracking the
// peak number of concurrent Fetch calls.
type slowCountingDiscovery struct {
	mu      sync.Mutex
	current int
	peak    int
	delay   time.Duration
}

func (s *slowCountingDiscovery) Fetch(ctx context.Context, _ string) (*DiscoveryDoc, error) {
	s.mu.Lock()
	s.current++
	if s.current > s.peak {
		s.peak = s.current
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.current--
	}()
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	return nil, ErrDiscoveryFailed
}

func (s *slowCountingDiscovery) peakConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// panickyDiscovery panics on every Fetch for uris in panicOn and delegates
// to inner otherwise. panicsLeft bounds the panics so retry-based callers
// can observe recovery.
type panickyDiscovery struct {
	mu         sync.Mutex
	inner      Discovery
	panicOn    map[string]bool
	panicsLeft int
}

func (p *panickyDiscovery) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
	p.mu.Lock()
	shouldPanic := p.panicOn[uri] && p.panicsLeft != 0
	if shouldPanic && p.panicsLeft > 0 {
		p.panicsLeft--
	}
	p.mu.Unlock()
	if shouldPanic {
		panic("discovery exploded")
	}
	return p.inner.Fetch(ctx, uri)
}

func TestRegistry_WarmJWKS_SurvivesPanickingFetch(t *testing.T) {
	// A panic inside one provider's warm-up must not crash the process (the
	// worker pool runs on its own goroutines) and must not prevent the other
	// providers from warming.
	idp := NewFixtureIdP(t)
	goodURI := idp.Issuer + "/.well-known/openid-configuration"
	badURI := "https://exploding.example/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &panickyDiscovery{
		inner: &flakyDiscovery{docs: map[string]*DiscoveryDoc{
			goodURI: {Issuer: idp.Issuer, JWKSURI: idp.JWKSURI},
		}},
		panicOn:    map[string]bool{badURI: true},
		panicsLeft: -1, // panic forever
	}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})

	plantProvider(t, r, testProvider(idp.Issuer, goodURI))
	plantProvider(t, r, testProvider("https://exploding.example", badURI))

	ctx := context.Background()
	if err := r.LoadProvidersFromKV(ctx); err != nil {
		t.Fatalf("LoadProvidersFromKV: %v", err)
	}
	r.WarmJWKS(ctx) // must not crash

	if _, err := r.ResolveKey("default", idp.Issuer, ""); err != nil {
		t.Fatalf("healthy provider not warmed alongside a panicking one: %v", err)
	}
}

func TestRegistry_WarmupRetryLoop_SurvivesPanickingFetch(t *testing.T) {
	// A panic during one retry tick must not crash the process or kill the
	// loop: once the discovery stops panicking, the provider still warms.
	idp := NewFixtureIdP(t)
	uri := idp.Issuer + "/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &panickyDiscovery{
		inner: &flakyDiscovery{docs: map[string]*DiscoveryDoc{
			uri: {Issuer: idp.Issuer, JWKSURI: idp.JWKSURI},
		}},
		panicOn:    map[string]bool{uri: true},
		panicsLeft: 2,
	}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true, WarmupRetryInterval: 20 * time.Millisecond})

	plantProvider(t, r, testProvider(idp.Issuer, uri))
	ctx := context.Background()
	if err := r.LoadProvidersFromKV(ctx); err != nil {
		t.Fatalf("LoadProvidersFromKV: %v", err)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	r.StartWarmupRetryLoop(loopCtx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.ResolveKey("default", idp.Issuer, ""); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("provider never warmed after panicking ticks (loop died?)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRegistry_ReloadAll_CarriedSourceForInactiveProviderDoesNotAuthenticate(t *testing.T) {
	// Fail-closed side of the carry-over: if the fresh store snapshot says a
	// provider is invalidated, its carried key source must not authenticate,
	// even though the source itself survived the swap.
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	p := testProvider("https://idp.example", "https://idp.example/.well-known/openid-configuration")
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey},
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"})

	invalidated := *p
	invalidatedAt := time.Now()
	invalidated.InvalidatedAt = &invalidatedAt
	plantProvider(t, r, &invalidated)

	if _, err := r.ResolveKey("k1", "https://idp.example", ""); err != nil {
		t.Fatalf("baseline ResolveKey: %v", err)
	}

	if err := r.ReloadAll(context.Background()); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	if _, err := r.ResolveKey("k1", "https://idp.example", ""); err == nil {
		t.Fatal("ResolveKey succeeded for an invalidated provider after ReloadAll (fail-closed violated)")
	}
}

func TestRegistry_WarmupRetryLoop_WarmsWhenIdPComesUp(t *testing.T) {
	// The one-shot startup warm-up loses the race against an IdP that boots
	// later than cyoda. The retry loop must keep re-attempting the warm-up
	// until the IdP is reachable — without any lifecycle call or restart.
	idp := NewFixtureIdP(t)
	uri := idp.Issuer + "/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{err: ErrDiscoveryFailed}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true, WarmupRetryInterval: 20 * time.Millisecond})

	p := testProvider(idp.Issuer, uri)
	plantProvider(t, r, p)
	ctx := context.Background()
	if err := r.LoadProvidersFromKV(ctx); err != nil {
		t.Fatalf("LoadProvidersFromKV: %v", err)
	}
	r.WarmJWKS(ctx) // one-shot warm-up fails: IdP still down

	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	r.StartWarmupRetryLoop(loopCtx)

	if _, err := r.ResolveKey("default", idp.Issuer, ""); !errors.Is(err, auth.ErrUnknownKID) {
		t.Fatalf("ResolveKey while IdP down = %v, want ErrUnknownKID", err)
	}

	// IdP comes up.
	disc.set(nil, map[string]*DiscoveryDoc{uri: {Issuer: idp.Issuer, JWKSURI: idp.JWKSURI}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.ResolveKey("default", idp.Issuer, ""); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("provider never warmed by the retry loop after the IdP came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRegistry_WarmupRetryLoop_SkipsWarmAndInactiveProviders(t *testing.T) {
	// The retry loop must not refetch providers that are already warm, nor
	// invalidated providers — only active, cold ones.
	warmIdP := NewFixtureIdP(t)
	warmURI := warmIdP.Issuer + "/.well-known/openid-configuration"
	inactiveURI := "https://inactive.example/.well-known/openid-configuration"
	store := newTestStore(t)
	// The inactive provider's discovery doc is deliberately absent during the
	// one-shot warm-up so its coordinate stays cold — otherwise it would be
	// skipped as "already warm" and never exercise the active-only check.
	disc := &flakyDiscovery{docs: map[string]*DiscoveryDoc{
		warmURI: {Issuer: warmIdP.Issuer, JWKSURI: warmIdP.JWKSURI},
	}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true, WarmupRetryInterval: 20 * time.Millisecond})

	warm := testProvider(warmIdP.Issuer, warmURI)
	plantProvider(t, r, warm)
	inactive := testProvider("https://inactive.example", inactiveURI)
	invalidatedAt := time.Now()
	inactive.InvalidatedAt = &invalidatedAt
	plantProvider(t, r, inactive)

	ctx := context.Background()
	if err := r.LoadProvidersFromKV(ctx); err != nil {
		t.Fatalf("LoadProvidersFromKV: %v", err)
	}
	r.WarmJWKS(ctx) // warms warmURI; inactiveURI fetch fails (no doc yet)
	warmBaseline := disc.fetchCount(warmURI)
	inactiveBaseline := disc.fetchCount(inactiveURI)

	// From here on the inactive provider's doc IS fetchable — only the
	// active-only check keeps the loop away from it.
	disc.set(nil, map[string]*DiscoveryDoc{
		warmURI:     {Issuer: warmIdP.Issuer, JWKSURI: warmIdP.JWKSURI},
		inactiveURI: {Issuer: "https://inactive.example", JWKSURI: "https://inactive.example/jwks"},
	})

	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	r.StartWarmupRetryLoop(loopCtx)

	time.Sleep(200 * time.Millisecond) // ~10 ticks
	if n := disc.fetchCount(warmURI); n != warmBaseline {
		t.Errorf("retry loop refetched a warm provider: %d fetches after baseline %d", n, warmBaseline)
	}
	if n := disc.fetchCount(inactiveURI); n != inactiveBaseline {
		t.Errorf("retry loop fetched an invalidated provider: %d fetches after baseline %d", n, inactiveBaseline)
	}
}

func TestHandleBroadcast_ReloadAllWarmsColdProviders(t *testing.T) {
	// The cluster reload_all broadcast is the peer-side of the reload
	// endpoint and must force-warm too, not just swap the provider map.
	idp := NewFixtureIdP(t)
	uri := idp.Issuer + "/.well-known/openid-configuration"
	store := newTestStore(t)
	disc := &flakyDiscovery{docs: map[string]*DiscoveryDoc{
		uri: {Issuer: idp.Issuer, JWKSURI: idp.JWKSURI},
	}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil,
		RegistryConfig{AllowPrivateNetworks: true})

	p := testProvider(idp.Issuer, uri)
	plantProvider(t, r, p)

	r.handleBroadcast([]byte(`{"op":"reload_all"}`))

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.ResolveKey("default", idp.Issuer, ""); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("provider never warmed after reload_all broadcast")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
