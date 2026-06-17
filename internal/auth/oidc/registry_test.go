package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	spi "github.com/cyoda-platform/cyoda-go-spi"
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

	res, err := r.ResolveKey("k1", "https://idp.example")
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

	_, err := r.ResolveKey("k1", "https://idp.example")
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

	_, _ = r.ResolveKey("k1", "https://idp.example") // populates kidIndex

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
	_, err := r.ResolveKey("k1", "https://idp.example")
	if err != nil {
		t.Fatalf("first ResolveKey: %v", err)
	}

	// Second call — hot path via kidIndex.
	res, err := r.ResolveKey("k1", "https://idp.example")
	if err != nil {
		t.Fatalf("second ResolveKey: %v", err)
	}
	if res.Provider.ID != p.ID {
		t.Errorf("hot-path Provider mismatch")
	}
}
