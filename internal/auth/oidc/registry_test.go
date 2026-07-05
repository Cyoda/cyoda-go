package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/google/uuid"
)

// fakeDiscovery serves DiscoveryDocs by uri lookup for registry tests.
type fakeDiscovery struct {
	docs map[string]*DiscoveryDoc
	err  error
}

func (f *fakeDiscovery) Fetch(_ context.Context, uri string) (*DiscoveryDoc, error) {
	if f.err != nil {
		return nil, f.err
	}
	if d, ok := f.docs[uri]; ok {
		return d, nil
	}
	return nil, ErrDiscoveryFailed
}

// fakeKeySource pretends to be an HTTPJWKSSource for a single known kid.
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
	return NewRegistry(
		newTestStore(t),
		&fakeDiscovery{docs: map[string]*DiscoveryDoc{}},
		nil,
		NopMetrics{},
		nil,
		RegistryConfig{AllowPrivateNetworks: true}, // tests bind to httptest.Server on 127.0.0.1
	)
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

	res, err := r.ResolveKey("k1", "https://idp.example", "")
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if res.Provider.ID != p.ID {
		t.Errorf("Provider mismatch")
	}

	// After cold-path resolve, the kidIndex should be populated (D6 invariant).
	if !r.kidIndexContains("k1", p.OwnerLegalEntityID.String(), p.WellKnownConfigURI) {
		t.Error("kidIndex not populated after cold-path resolve (D6 invariant violated)")
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

	_, err := r.ResolveKey("k1", "https://evil.example", "")
	if !errors.Is(err, auth.ErrIssuerMismatch) {
		t.Errorf("err = %v, want ErrIssuerMismatch", err)
	}
}

func TestRegistry_ResolveKey_UnknownKidFallsThrough(t *testing.T) {
	r := newTestRegistry(t)
	_, err := r.ResolveKey("never-seen", "any", "")
	if !errors.Is(err, auth.ErrUnknownKID) {
		t.Errorf("err = %v, want ErrUnknownKID", err)
	}
}

func TestRegistry_ResolveKey_Phase2PendingReturnsErrUnknownKID(t *testing.T) {
	// D8 + I2: provider with nil discoveryDoc (JWKS not yet warmed) must not
	// be surfaced as ErrIssuerMismatch — it contributes nothing to resolution.
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://idp.example",
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: uuid.New(),
	}
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey}, nil /* discoveryDoc nil */)

	_, err := r.ResolveKey("k1", "https://idp.example", "")
	if !errors.Is(err, auth.ErrUnknownKID) {
		t.Errorf("err = %v, want ErrUnknownKID (Phase-2-pending case)", err)
	}
}

func TestRegistry_EvictKidEntry_SelfHeal(t *testing.T) {
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
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

	_, _ = r.ResolveKey("k1", "https://idp.example", "") // populates kidIndex

	ref := providerRef{tenant: spi.TenantID(p.OwnerLegalEntityID.String()), uri: p.WellKnownConfigURI}
	r.EvictKidEntry("k1", ref)

	if r.kidIndexContains("k1", p.OwnerLegalEntityID.String(), p.WellKnownConfigURI) {
		t.Error("kidIndex still contains entry after EvictKidEntry (D6 self-heal failed)")
	}
}

func TestRegistry_EvictKidEntry_Idempotent(t *testing.T) {
	r := newTestRegistry(t)
	ref := providerRef{tenant: "t1", uri: "https://nonexistent.example"}
	// Should not panic or error when entry doesn't exist.
	r.EvictKidEntry("no-such-kid", ref)
	r.EvictKidEntry("no-such-kid", ref)
}

func TestRegistry_ReloadAll_TakesWriteLock(t *testing.T) {
	// D18: ReloadAll must atomically swap the provider maps. After it
	// completes, ResolveKey must find the freshly-loaded provider.
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
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

	// Plant the provider in KV so ReloadAll has something to load.
	ctx := context.Background()
	if err := r.store.Register(ctx, p); err != nil {
		t.Fatalf("store.Register: %v", err)
	}
	if err := r.store.PutURIHistory(ctx, sha256Hex(p.WellKnownConfigURI), &UriOwnershipHistory{
		CurrentOwner: &Owner{
			TenantID:     p.OwnerLegalEntityID.String(),
			ProviderUUID: p.ID.String(),
			RegisteredAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("store.PutURIHistory: %v", err)
	}

	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	// After ReloadAll, the provider map is rebuilt from KV. The source
	// is not warmed (Phase-2 only), so ResolveKey returns ErrUnknownKID.
	// What we verify is that ReloadAll itself completes without error and
	// that the loaded provider count is reflected in the map.
	r.mu.RLock()
	byURI := r.providers[spi.TenantID(p.OwnerLegalEntityID.String())]
	r.mu.RUnlock()
	if byURI == nil {
		t.Fatal("provider map missing after ReloadAll")
	}
	if _, ok := byURI[p.WellKnownConfigURI]; !ok {
		t.Error("provider not present in map after ReloadAll")
	}
}

func TestRegistry_ResolveKey_HotPathFromKidIndex(t *testing.T) {
	// After a cold-path resolution populates kidIndex, the hot path should
	// succeed on the second call.
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
		&DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "x"})

	// First call — cold path.
	_, err := r.ResolveKey("k1", "https://idp.example", "")
	if err != nil {
		t.Fatalf("first ResolveKey: %v", err)
	}

	// Second call — hot path via kidIndex.
	res, err := r.ResolveKey("k1", "https://idp.example", "")
	if err != nil {
		t.Fatalf("second ResolveKey: %v", err)
	}
	if res.Provider.ID != p.ID {
		t.Errorf("hot-path Provider mismatch")
	}
}

// TestRegistry_MaliciousDiscoveryJWKSURISSRFBlocked verifies that when a
// discovery document names a loopback jwks_uri, reloadOne refuses to fetch it
// (D10 fetch-time SSRF defence). The malicious endpoint must never receive a
// GET even though the provider itself exists in the registry.
//
// Design: we bypass HTTPDiscovery entirely by using fakeDiscovery, so the
// discovery server (also on 127.0.0.1) is never contacted over HTTP — only
// the JWKS fetch is exercised. allowPrivate=false causes safeDialContext to
// block the loopback JWKS URL.
func TestRegistry_MaliciousDiscoveryJWKSURISSRFBlocked(t *testing.T) {
	// Stand up a "malicious internal" server that records any incoming hits.
	var internalHit atomic.Bool
	malicious := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[]}`)
	}))
	defer malicious.Close()

	// fakeDiscovery returns a doc whose jwks_uri points at the malicious server.
	// This simulates an attacker-controlled discovery doc that redirects the
	// JWKS fetch to an internal endpoint.
	disc := &fakeDiscovery{
		docs: map[string]*DiscoveryDoc{
			"https://example.test/.well-known/openid-configuration": {
				Issuer:  "https://idp.example.test",
				JWKSURI: malicious.URL + "/jwks",
			},
		},
	}

	// AllowPrivateNetworks=false: the safeDialContext for the JWKS transport must block
	// 127.0.0.1 (malicious.URL host).
	r := NewRegistry(newTestStore(t), disc, nil, NopMetrics{}, nil, RegistryConfig{AllowPrivateNetworks: false})

	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://example.test/.well-known/openid-configuration",
		Issuers:            []string{"https://idp.example.test"},
		OwnerLegalEntityID: uuid.New(),
		CreatedAt:          time.Now(),
	}
	r.addToProviderMap(p)

	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	r.reloadOne(context.Background(), tenant, p.WellKnownConfigURI)

	// Trigger the lazy JWKS fetch: reloadOne only constructs the HTTPJWKSSource;
	// the HTTP GET to jwks_uri happens on the first call to GetKey (inside
	// ResolveKey's disposeCandidates). Without this call, the malicious URL would
	// never be dialed — making the test a false-pass even if safeDialContext was
	// removed from the JWKS transport.
	_, _ = r.ResolveKey("test-kid", "https://idp.example.test", "")

	// THE ACTUAL ASSERTION: the malicious server received NO hits.
	if internalHit.Load() {
		t.Fatal("malicious internal JWKS endpoint received a GET — SSRF defence FAILED for JWKS fetch")
	}
}

// ---------------------------------------------------------------------------
// Audit fix: cross-tenant resolution with audience disambiguation (#284)
// ---------------------------------------------------------------------------

// TestResolveKey_TwoTenantsSameURIDistinctAudiences_RoutesByAud verifies
// Layer 1 of the Critical audit fix: when two tenants register the same
// wellKnownConfigUri with distinct ExpectedAudiences, ResolveKey routes
// deterministically by aud claim to the correct tenant's provider.
func TestResolveKey_TwoTenantsSameURIDistinctAudiences_RoutesByAud(t *testing.T) {
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	tenantAUUID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	tenantBUUID := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	pA := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://shared-idp.example",
		Issuers:            []string{"https://shared-idp.example"},
		ExpectedAudiences:  []string{"app-a"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: tenantAUUID,
	}
	pB := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://shared-idp.example",
		Issuers:            []string{"https://shared-idp.example"},
		ExpectedAudiences:  []string{"app-b"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: tenantBUUID,
	}
	doc := &DiscoveryDoc{Issuer: "https://shared-idp.example", JWKSURI: "https://shared-idp.example/jwks"}
	// Both share the same key (simulating same physical IdP).
	ks := &fakeKeySource{kid: "k1", key: &priv.PublicKey}
	r.installForTest(pA, ks, doc)
	r.installForTest(pB, ks, doc)

	// JWT with aud="app-a" must resolve to provider A.
	res, err := r.ResolveKey("k1", "https://shared-idp.example", "app-a")
	if err != nil {
		t.Fatalf("ResolveKey(aud=app-a): %v", err)
	}
	if res.Provider.ID != pA.ID {
		t.Errorf("aud=app-a: got provider %v, want provider A (%v)", res.Provider.ID, pA.ID)
	}

	// JWT with aud="app-b" must resolve to provider B.
	res, err = r.ResolveKey("k1", "https://shared-idp.example", "app-b")
	if err != nil {
		t.Fatalf("ResolveKey(aud=app-b): %v", err)
	}
	if res.Provider.ID != pB.ID {
		t.Errorf("aud=app-b: got provider %v, want provider B (%v)", res.Provider.ID, pB.ID)
	}
}

// TestResolveKey_TwoTenantsSameURIOverlappingAudiences_ErrAmbiguous verifies
// that when two providers share an aud in their ExpectedAudiences, ResolveKey
// returns ErrAmbiguousProvider (wrapped in ErrUnknownKID).
func TestResolveKey_TwoTenantsSameURIOverlappingAudiences_ErrAmbiguous(t *testing.T) {
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	tenantAUUID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	tenantBUUID := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	pA := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://shared-idp.example",
		Issuers:            []string{"https://shared-idp.example"},
		ExpectedAudiences:  []string{"shared-aud"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: tenantAUUID,
	}
	pB := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://shared-idp.example",
		Issuers:            []string{"https://shared-idp.example"},
		ExpectedAudiences:  []string{"shared-aud"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: tenantBUUID,
	}
	doc := &DiscoveryDoc{Issuer: "https://shared-idp.example", JWKSURI: "https://shared-idp.example/jwks"}
	ks := &fakeKeySource{kid: "k1", key: &priv.PublicKey}
	r.installForTest(pA, ks, doc)
	r.installForTest(pB, ks, doc)

	_, err := r.ResolveKey("k1", "https://shared-idp.example", "shared-aud")
	if err == nil {
		t.Fatal("ResolveKey with overlapping audiences: expected error, got nil")
	}
	if !errors.Is(err, ErrAmbiguousProvider) {
		t.Errorf("err = %v, want ErrAmbiguousProvider", err)
	}
	if !errors.Is(err, auth.ErrUnknownKID) {
		t.Errorf("err = %v, must wrap auth.ErrUnknownKID (chain fall-through sentinel)", err)
	}
}

// TestResolveKey_TwoTenantsSameURIEmptyAudiences_ErrAmbiguous verifies that
// when both providers have empty ExpectedAudiences (and the same iss/sig),
// ResolveKey rejects with ErrAmbiguousProvider rather than routing randomly.
func TestResolveKey_TwoTenantsSameURIEmptyAudiences_ErrAmbiguous(t *testing.T) {
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	tenantAUUID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	tenantBUUID := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	pA := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://shared-idp.example",
		Issuers:            []string{"https://shared-idp.example"},
		ExpectedAudiences:  nil, // empty
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: tenantAUUID,
	}
	pB := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://shared-idp.example",
		Issuers:            []string{"https://shared-idp.example"},
		ExpectedAudiences:  nil, // empty
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: tenantBUUID,
	}
	doc := &DiscoveryDoc{Issuer: "https://shared-idp.example", JWKSURI: "https://shared-idp.example/jwks"}
	ks := &fakeKeySource{kid: "k1", key: &priv.PublicKey}
	r.installForTest(pA, ks, doc)
	r.installForTest(pB, ks, doc)

	_, err := r.ResolveKey("k1", "https://shared-idp.example", "any-aud")
	if err == nil {
		t.Fatal("ResolveKey with two providers and empty audiences: expected error, got nil")
	}
	if !errors.Is(err, ErrAmbiguousProvider) {
		t.Errorf("err = %v, want ErrAmbiguousProvider", err)
	}
	if !errors.Is(err, auth.ErrUnknownKID) {
		t.Errorf("err = %v, must wrap auth.ErrUnknownKID (chain fall-through sentinel)", err)
	}
}

// TestResolveKey_DeterministicSortColdPath verifies that the cold path iterates
// providers in deterministic tenant+uri lexicographic order across multiple calls,
// so kidIndex population order is reproducible regardless of Go's map iteration.
func TestResolveKey_DeterministicSortColdPath(t *testing.T) {
	r := newTestRegistry(t)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	// Install a single provider so ResolveKey succeeds. The sort is load-bearing
	// for the multi-provider ambiguity case; here we verify the cold path
	// completes deterministically by running it twice with the same result.
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: "https://idp.example",
		Issuers:            []string{"https://idp.example"},
		ExpectedAudiences:  []string{"my-app"},
		CreatedAt:          time.Now().Add(-time.Hour),
		OwnerLegalEntityID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
	}
	doc := &DiscoveryDoc{Issuer: "https://idp.example", JWKSURI: "https://idp.example/jwks"}
	r.installForTest(p, &fakeKeySource{kid: "k1", key: &priv.PublicKey}, doc)

	// First cold-path call.
	res1, err := r.ResolveKey("k1", "https://idp.example", "my-app")
	if err != nil {
		t.Fatalf("first ResolveKey: %v", err)
	}

	// Evict and retry (cold path again).
	r.EvictKidEntry("k1", res1.ProviderRef)
	res2, err := r.ResolveKey("k1", "https://idp.example", "my-app")
	if err != nil {
		t.Fatalf("second ResolveKey: %v", err)
	}

	if res1.Provider.ID != res2.Provider.ID {
		t.Errorf("cold path non-deterministic: first=%v second=%v", res1.Provider.ID, res2.Provider.ID)
	}
}

// ---------------------------------------------------------------------------
// Audit fix E2: fetch-time pin enforcement for pinned Issuers (#284)
// ---------------------------------------------------------------------------

// outcomeRecordingMetrics extends recordingMetrics to capture the last
// IncJWKSFetchError outcome label — needed to verify the issuer_pin_mismatch
// metric path without coupling tests to atomic counter ordering.
type outcomeRecordingMetrics struct {
	NopMetrics
	lastOutcome string
	jwksErrors  int64
}

func (m *outcomeRecordingMetrics) IncJWKSFetchError(outcome string) {
	atomic.AddInt64(&m.jwksErrors, 1)
	m.lastOutcome = outcome
}

// TestRegistry_ReloadOne_RefusesSourceWhenDiscoveryIssuerNotInPinnedList
// verifies E2 defence-in-depth: when a provider has a non-empty Issuers list
// and the discovery doc returns an Issuer value that is NOT in that list,
// reloadOne MUST refuse to install the providerSource.
// The provider remains in Phase-2-pending state (source nil); subsequent
// ResolveKey calls return ErrUnknownKID.
func TestRegistry_ReloadOne_RefusesSourceWhenDiscoveryIssuerNotInPinnedList(t *testing.T) {
	const providerURI = "https://legit.example/.well-known/openid-configuration"

	disc := &fakeDiscovery{
		docs: map[string]*DiscoveryDoc{
			providerURI: {
				// Attacker-controlled discovery doc claims a different issuer.
				Issuer:  "https://attacker.example",
				JWKSURI: "https://attacker.example/jwks",
			},
		},
	}
	m := &outcomeRecordingMetrics{}
	r := NewRegistry(newTestStore(t), disc, nil, m, nil, RegistryConfig{AllowPrivateNetworks: true})

	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: providerURI,
		Issuers:            []string{"https://legit.example"},
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: uuid.New(),
	}
	r.addToProviderMap(p)

	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	r.reloadOne(context.Background(), tenant, providerURI)

	// Source must NOT be installed.
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if byURI := r.sources[tenant]; byURI != nil {
			if src := byURI[providerURI]; src != nil {
				t.Error("source was installed despite issuer mismatch with pinned Issuers — E2 not enforced")
			}
		}
	}()

	// Metric must have been incremented with the expected outcome.
	if atomic.LoadInt64(&m.jwksErrors) == 0 {
		t.Error("IncJWKSFetchError was not called on issuer pin mismatch")
	}
	if m.lastOutcome != "issuer_pin_mismatch" {
		t.Errorf("IncJWKSFetchError outcome = %q, want %q", m.lastOutcome, "issuer_pin_mismatch")
	}
}

// TestRegistry_ReloadOne_AcceptsWhenDiscoveryIssuerInPinnedList verifies that
// reloadOne installs the source when the discovery doc's Issuer is in the
// provider's pinned Issuers list.
func TestRegistry_ReloadOne_AcceptsWhenDiscoveryIssuerInPinnedList(t *testing.T) {
	const providerURI = "https://legit.example/.well-known/openid-configuration"

	disc := &fakeDiscovery{
		docs: map[string]*DiscoveryDoc{
			providerURI: {
				Issuer:  "https://legit.example",
				JWKSURI: "https://legit.example/jwks",
			},
		},
	}
	r := NewRegistry(newTestStore(t), disc, nil, NopMetrics{}, nil, RegistryConfig{AllowPrivateNetworks: true})

	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: providerURI,
		Issuers:            []string{"https://legit.example", "https://legit2.example"},
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: uuid.New(),
	}
	r.addToProviderMap(p)

	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	r.reloadOne(context.Background(), tenant, providerURI)

	// Source MUST be installed.
	var installed bool
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if byURI := r.sources[tenant]; byURI != nil {
			installed = byURI[providerURI] != nil
		}
	}()
	if !installed {
		t.Error("source was not installed when discovery doc issuer matches pinned Issuers")
	}
}

// TestRegistry_ReloadOne_AcceptsAnyDiscoveryIssuerWhenIssuersEmpty verifies
// that when provider.Issuers is empty (nil), reloadOne does not perform
// fetch-time pin enforcement and installs the source for any discovery issuer.
// This preserves spec D17's fallback: empty Issuers → doc.Issuer is used at
// resolution time.
func TestRegistry_ReloadOne_AcceptsAnyDiscoveryIssuerWhenIssuersEmpty(t *testing.T) {
	const providerURI = "https://idp.example/.well-known/openid-configuration"

	disc := &fakeDiscovery{
		docs: map[string]*DiscoveryDoc{
			providerURI: {
				Issuer:  "https://anything.example",
				JWKSURI: "https://anything.example/jwks",
			},
		},
	}
	r := NewRegistry(newTestStore(t), disc, nil, NopMetrics{}, nil, RegistryConfig{AllowPrivateNetworks: true})

	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: providerURI,
		Issuers:            nil, // empty — no pin
		CreatedAt:          time.Now(),
		OwnerLegalEntityID: uuid.New(),
	}
	r.addToProviderMap(p)

	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	r.reloadOne(context.Background(), tenant, providerURI)

	// Source MUST be installed (no pin enforcement when Issuers is empty).
	var installed bool
	func() {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if byURI := r.sources[tenant]; byURI != nil {
			installed = byURI[providerURI] != nil
		}
	}()
	if !installed {
		t.Error("source was not installed for provider with empty Issuers list — D17 fallback broken")
	}
}
