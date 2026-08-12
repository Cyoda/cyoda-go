package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/google/uuid"
)

// Tests for ReloadAll key-source preservation and force-warm semantics.
//
// Contract under test: POST /oauth/oidc/providers/reload must force-warm the
// JWKS of every provider it reloads, and must never leave the registry with
// fewer warm key sources than before the call. A reload against an
// unreachable IdP keeps serving the previously cached keys.

// flakyDiscovery is a mutable, concurrency-safe Discovery fake: err can be
// flipped at runtime to simulate an IdP that is down and later comes up.
type flakyDiscovery struct {
	mu      sync.Mutex
	docs    map[string]*DiscoveryDoc
	err     error
	fetches map[string]int
}

func (f *flakyDiscovery) Fetch(_ context.Context, uri string) (*DiscoveryDoc, error) {
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
	r.WarmJWKSAsync(ctx) // one-shot warm-up fails: IdP still down

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
	r.WarmJWKSAsync(ctx) // warms warmURI; inactiveURI fetch fails (no doc yet)
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
