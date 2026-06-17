package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"

	memplugin "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newTestKV(t *testing.T) spi.KeyValueStore {
	t.Helper()
	factory := memplugin.NewStoreFactory()
	systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "system",
		UserName: "System",
		Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
	})
	kv, err := factory.KeyValueStore(systemCtx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	return kv
}

func newTestStore(t *testing.T) *KVOidcProviderStore {
	t.Helper()
	systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "system",
		UserName: "System",
		Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
	})
	s, err := NewKVProviderStore(systemCtx, newTestKV(t))
	if err != nil {
		t.Fatalf("NewKVProviderStore: %v", err)
	}
	return s
}

func sampleProvider(t *testing.T, tenant spi.TenantID, uri string) *OidcProvider {
	t.Helper()
	tenantUUID, err := uuid.Parse(string(tenant))
	if err != nil {
		// Allow non-UUID tenant strings in tests by using a deterministic
		// UUID for the owner. The store uses OwnerLegalEntityID for
		// stale-index defence, so it must match the tenant in tests.
		t.Fatalf("test tenant must be a valid UUID string: %v", err)
	}
	return &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		Issuers:            []string{"https://idp.example"},
		CreatedAt:          time.Now().UTC().Truncate(time.Second),
		OwnerLegalEntityID: tenantUUID,
	}
}

func TestKVStore_RegisterAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenantA := spi.TenantID("00000000-0000-0000-0000-00000000000a")

	p := sampleProvider(t, tenantA, "https://idp-a.example/.well-known/openid-configuration")
	if err := s.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := s.Get(ctx, tenantA, p.ID.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.WellKnownConfigURI != p.WellKnownConfigURI {
		t.Errorf("URI mismatch: got %s, want %s", got.WellKnownConfigURI, p.WellKnownConfigURI)
	}
}

func TestKVStore_GetByURI(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenantA := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	uri := "https://idp-a.example/.well-known/openid-configuration"

	p := sampleProvider(t, tenantA, uri)
	if err := s.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := s.GetByURI(ctx, tenantA, uri)
	if err != nil {
		t.Fatalf("GetByURI: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID mismatch: got %s, want %s", got.ID, p.ID)
	}
}

func TestKVStore_CrossTenantGetReturns404(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenantA := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	tenantB := spi.TenantID("00000000-0000-0000-0000-00000000000b")

	p := sampleProvider(t, tenantA, "https://idp.example/.well-known/openid-configuration")
	if err := s.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := s.Get(ctx, tenantB, p.ID.String())
	if err == nil {
		t.Fatal("expected ErrProviderNotFound, got nil")
	}
}

func TestKVStore_ListByTenant_FiltersOtherTenants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenantA := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	tenantB := spi.TenantID("00000000-0000-0000-0000-00000000000b")

	_ = s.Register(ctx, sampleProvider(t, tenantA, "https://a1.example"))
	_ = s.Register(ctx, sampleProvider(t, tenantA, "https://a2.example"))
	_ = s.Register(ctx, sampleProvider(t, tenantB, "https://b1.example"))

	got, err := s.ListByTenant(ctx, tenantA, false)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d providers, want 2", len(got))
	}
}

func TestKVStore_LoadAll_AcrossTenants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Register(ctx, sampleProvider(t, spi.TenantID("00000000-0000-0000-0000-00000000000a"), "https://a.example"))
	_ = s.Register(ctx, sampleProvider(t, spi.TenantID("00000000-0000-0000-0000-00000000000b"), "https://b.example"))
	_ = s.Register(ctx, sampleProvider(t, spi.TenantID("00000000-0000-0000-0000-00000000000c"), "https://c.example"))

	all, err := s.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("LoadAll = %d, want 3", len(all))
	}
}

func TestKVStore_URIHistoryRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uri := "https://idp.example/.well-known/openid-configuration"

	got, err := s.GetURIHistory(ctx, sha256Hex(uri))
	if err != nil {
		t.Fatalf("GetURIHistory: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for first read, got %+v", got)
	}

	now := time.Now().UTC().Truncate(time.Second)
	h := &UriOwnershipHistory{
		CurrentOwner: &Owner{TenantID: "a", ProviderUUID: "u1", RegisteredAt: now},
	}
	if err := s.PutURIHistory(ctx, sha256Hex(uri), h); err != nil {
		t.Fatalf("PutURIHistory: %v", err)
	}
	back, err := s.GetURIHistory(ctx, sha256Hex(uri))
	if err != nil || back == nil {
		t.Fatalf("GetURIHistory after Put: back=%v err=%v", back, err)
	}
	if back.CurrentOwner.TenantID != "a" {
		t.Errorf("history CurrentOwner lost")
	}
}

func TestKVStore_DeleteRemovesBlobAndIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenant := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	p := sampleProvider(t, tenant, "https://idp.example")
	_ = s.Register(ctx, p)

	if err := s.Delete(ctx, tenant, p.ID.String(), p.WellKnownConfigURI); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := s.Get(ctx, tenant, p.ID.String()); err == nil {
		t.Error("expected not-found after Delete")
	}
	if _, err := s.GetByURI(ctx, tenant, p.WellKnownConfigURI); err == nil {
		t.Error("expected not-found via index after Delete")
	}
}

func TestKVStore_RaceValidateIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenant := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	uri := "https://idp.example"

	p1 := sampleProvider(t, tenant, uri)
	if err := s.Register(ctx, p1); err != nil {
		t.Fatalf("Register p1: %v", err)
	}

	// Simulate a "loser" who wrote the index later thinking p2's UUID is the winner.
	winningID, ok, err := s.RaceValidateIndex(ctx, tenant, uri, "some-other-uuid")
	if err != nil {
		t.Fatalf("RaceValidateIndex: %v", err)
	}
	if ok {
		t.Error("expected ok=false (loser)")
	}
	if winningID != p1.ID.String() {
		t.Errorf("winningID = %s, want %s", winningID, p1.ID.String())
	}

	// Winner case: ID matches.
	_, ok, err = s.RaceValidateIndex(ctx, tenant, uri, p1.ID.String())
	if err != nil {
		t.Fatalf("RaceValidateIndex: %v", err)
	}
	if !ok {
		t.Error("expected ok=true (winner)")
	}
}

func TestKVStore_GetByURI_CleansUpOrphanIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenant := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	uri := "https://idp-orphan.example/.well-known/openid-configuration"

	// Manually plant an orphan index entry (no corresponding blob).
	orphanID := uuid.New().String()
	indexKey := uriIndexKey(tenant, uri)
	if err := s.kv.Put(s.ctx, Namespace, indexKey, []byte(orphanID)); err != nil {
		t.Fatalf("Put orphan index: %v", err)
	}

	_, err := s.GetByURI(ctx, tenant, uri)
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("expected ErrProviderNotFound, got %v", err)
	}

	// Verify cleanup: the orphan index should be gone.
	if _, getErr := s.kv.Get(s.ctx, Namespace, indexKey); !errors.Is(getErr, spi.ErrNotFound) {
		t.Errorf("orphan index not cleaned up; Get returned %v", getErr)
	}
}

func TestKVStore_ListByTenant_FiltersBlobsWithStaleOwner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenantA := spi.TenantID("00000000-0000-0000-0000-00000000000a")
	tenantB := spi.TenantID("00000000-0000-0000-0000-00000000000b")

	// One legitimate provider for tenant A.
	good := sampleProvider(t, tenantA, "https://good.example")
	if err := s.Register(ctx, good); err != nil {
		t.Fatalf("Register good: %v", err)
	}

	// Plant a blob under tenant A's key prefix whose Owner is tenant B (stale).
	stale := sampleProvider(t, tenantB, "https://stale.example")
	stale.OwnerLegalEntityID = uuid.MustParse(string(tenantB))
	blob, _ := json.Marshal(stale)
	if err := s.kv.Put(s.ctx, Namespace, providerBlobKey(tenantA, stale.ID.String()), blob); err != nil {
		t.Fatalf("Put stale blob: %v", err)
	}

	got, err := s.ListByTenant(ctx, tenantA, false)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("ListByTenant returned %d providers (want 1, stale must be filtered)", len(got))
	}
	if len(got) >= 1 && got[0].ID != good.ID {
		t.Errorf("ListByTenant returned wrong provider: got %v, want %v", got[0].ID, good.ID)
	}
}
