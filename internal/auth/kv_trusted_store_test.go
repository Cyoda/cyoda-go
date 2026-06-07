package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// systemCtx returns a context with the SYSTEM tenant, used for KV operations.
func systemCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "system",
		UserName: "System",
		Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
	})
}

func TestKVTrustedKeyStore_PersistsAcrossInstances(t *testing.T) {
	// Shared KV backend (simulates restart — same storage, new store instance).
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	expiry := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	tk := &auth.TrustedKey{
		KID:       "persist-key-1",
		TenantID:  spi.SystemTenantID,
		PublicKey: &key1.PublicKey,
		Audience:  "api://my-service",
		Issuers:   []string{"https://issuer.example.com", "https://backup-issuer.example.com"},
		Active:    true,
		ValidFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:   &expiry,
	}

	// --- Instance 1: register key ---
	store1, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 1): %v", err)
	}

	if err := store1.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Verify it's accessible on instance 1.
	got, err := store1.Get(spi.SystemTenantID, "persist-key-1")
	if err != nil {
		t.Fatalf("Get on instance 1: %v", err)
	}
	if got.KID != "persist-key-1" || got.Audience != "api://my-service" || !got.Active {
		t.Errorf("instance 1: unexpected key: KID=%s Audience=%s Active=%v", got.KID, got.Audience, got.Active)
	}

	// --- Instance 2: new store from same KV backend (simulates restart) ---
	store2, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 2): %v", err)
	}

	got2, err := store2.Get(spi.SystemTenantID, "persist-key-1")
	if err != nil {
		t.Fatalf("Get on instance 2 (after simulated restart): %v", err)
	}
	if got2.KID != "persist-key-1" {
		t.Errorf("expected KID persist-key-1, got %s", got2.KID)
	}
	if got2.Audience != "api://my-service" {
		t.Errorf("expected audience api://my-service, got %s", got2.Audience)
	}
	if !got2.Active {
		t.Error("expected key to be active")
	}
	if len(got2.Issuers) != 2 || got2.Issuers[0] != "https://issuer.example.com" {
		t.Errorf("expected 2 issuers, got %v", got2.Issuers)
	}
	if got2.ValidFrom != tk.ValidFrom {
		t.Errorf("expected ValidFrom %v, got %v", tk.ValidFrom, got2.ValidFrom)
	}
	if got2.ValidTo == nil || *got2.ValidTo != expiry {
		t.Errorf("expected ValidTo %v, got %v", expiry, got2.ValidTo)
	}
	// Verify the RSA public key round-trips correctly.
	if got2.PublicKey == nil {
		t.Fatal("expected non-nil PublicKey")
	}
	if got2.PublicKey.N.Cmp(key1.PublicKey.N) != 0 || got2.PublicKey.E != key1.PublicKey.E {
		t.Error("public key mismatch after round-trip")
	}
}

// TestKVTrustedKeyStore_CrossNodeVisibility simulates two nodes sharing the same
// KV backend. A key registered on node-1's store must be visible on node-2's
// store WITHOUT restarting node-2. This is the multi-node OBO token exchange bug:
// client registers trusted key via node-1 (LB), token exchange hits node-2 (LB),
// node-2's cache doesn't have the key → "unknown trusted key".
func TestKVTrustedKeyStore_CrossNodeVisibility(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)

	// Both stores created from the same KV backend (simulates two nodes at startup)
	store1, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (node-1): %v", err)
	}
	store2, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (node-2): %v", err)
	}

	// Register key on node-1 AFTER both stores are created
	tk := &auth.TrustedKey{
		KID:       "cross-node-key",
		TenantID:  spi.SystemTenantID,
		PublicKey: &key1.PublicKey,
		Audience:  "api://test",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}
	if err := store1.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register on node-1: %v", err)
	}

	// Node-2 must see the key without restart
	got, err := store2.Get(spi.SystemTenantID, "cross-node-key")
	if err != nil {
		t.Fatalf("Get on node-2 should find key registered on node-1: %v", err)
	}
	if got.KID != "cross-node-key" {
		t.Errorf("KID = %q, want %q", got.KID, "cross-node-key")
	}
}

func TestKVTrustedKeyStore_DeletePersists(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	tk := &auth.TrustedKey{
		KID:       "del-key",
		TenantID:  spi.SystemTenantID,
		PublicKey: &key1.PublicKey,
		Audience:  "test",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}

	store1, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}
	if err := store1.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store1.Delete(spi.SystemTenantID, "del-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// New instance should not see the deleted key.
	store2, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 2): %v", err)
	}
	_, err = store2.Get(spi.SystemTenantID, "del-key")
	if err == nil {
		t.Fatal("expected error for deleted key on new instance, got nil")
	}
}

func TestKVTrustedKeyStore_InvalidateReactivatePersists(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	tk := &auth.TrustedKey{
		KID:       "toggle-key",
		TenantID:  spi.SystemTenantID,
		PublicKey: &key1.PublicKey,
		Audience:  "test",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}

	store1, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}
	if err := store1.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store1.Invalidate(spi.SystemTenantID, "toggle-key", 0); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	// New instance should see inactive key.
	store2, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 2): %v", err)
	}
	got, err := store2.Get(spi.SystemTenantID, "toggle-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Active {
		t.Error("expected key to be inactive after persist")
	}

	// Reactivate and verify persists.
	if err := store2.Reactivate(spi.SystemTenantID, "toggle-key", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	store3, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 3): %v", err)
	}
	got3, err := store3.Get(spi.SystemTenantID, "toggle-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got3.Active {
		t.Error("expected key to be active after reactivate persist")
	}
}

// TestKVTrustedKeyStore_RegisterRespectsMaxTrustedKeys verifies that the store
// rejects Register once the configured cap is reached — defence against
// memory/storage exhaustion via runaway trusted-key registration (#34 item 2).
func TestKVTrustedKeyStore_RegisterRespectsMaxTrustedKeys(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	store, err := auth.NewKVTrustedKeyStore(ctx, kvStore, auth.WithMaxTrustedKeys(3))
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}

	for i := 0; i < 3; i++ {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		tk := &auth.TrustedKey{
			KID:       "cap-key-" + string(rune('a'+i)),
			TenantID:  spi.SystemTenantID,
			PublicKey: &key.PublicKey,
			Audience:  "svc",
			Active:    true,
			ValidFrom: time.Now().UTC(),
		}
		if err := store.Register(tk, auth.RotateOptions{}); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}

	// Fourth registration must fail with a 409 Conflict AppError.
	overflowKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	overflow := &auth.TrustedKey{
		KID:       "cap-key-overflow",
		TenantID:  spi.SystemTenantID,
		PublicKey: &overflowKey.PublicKey,
		Audience:  "svc",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}
	err = store.Register(overflow, auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected Register to reject 4th key when MaxTrustedKeys=3, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeTrustedKeyCapReached)
	}
}

// TestKVTrustedKeyStore_RegisterUpsertsSameKID pins the cyoda-cloud trusted-key
// upsert contract. Same-tenant + same KID is the in-place replace path: the
// new JWK material atomically replaces the old record under the same KID.
// The endpoint is idempotent on KID — retrying a partially-failed
// registration must succeed, not 409.
func TestKVTrustedKeyStore_RegisterUpsertsSameKID(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	store, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}

	originalKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	original := &auth.TrustedKey{
		KID:       "rotate-kid",
		TenantID:  spi.SystemTenantID,
		PublicKey: &originalKey.PublicKey,
		Audience:  "original-aud",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}
	if err := store.Register(original, auth.RotateOptions{}); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Re-register with new JWK material under the same KID. Per the cloud
	// upsert contract this must succeed (not 409) and replace the record.
	rotatedKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rotated := &auth.TrustedKey{
		KID:       "rotate-kid",
		TenantID:  spi.SystemTenantID,
		PublicKey: &rotatedKey.PublicKey,
		Audience:  "rotated-aud",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}
	if err := store.Register(rotated, auth.RotateOptions{}); err != nil {
		t.Fatalf("re-Register (upsert): expected nil, got %v", err)
	}

	got, err := store.Get(spi.SystemTenantID, "rotate-kid")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Audience != "rotated-aud" {
		t.Errorf("audience = %q, want %q (upsert did not replace)", got.Audience, "rotated-aud")
	}
	if got.PublicKey.N.Cmp(rotatedKey.PublicKey.N) != 0 {
		t.Error("modulus mismatch — upsert did not replace the JWK material")
	}

	// Persistence: a fresh store loaded from the same KV must see the
	// rotated key, not the original.
	store2, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 2): %v", err)
	}
	got2, err := store2.Get(spi.SystemTenantID, "rotate-kid")
	if err != nil {
		t.Fatalf("Get on instance 2: %v", err)
	}
	if got2.Audience != "rotated-aud" {
		t.Errorf("persisted audience = %q, want %q", got2.Audience, "rotated-aud")
	}
}

// TestKVTrustedKeyStore_RegisterUpsertDoesNotConsumeCapSlot guards an
// adjacent invariant: an upsert (re-Register on an existing KID) must not
// be blocked by a full registry cap, because the result does not grow the
// registry. Without this, key rotation against a cap-saturated registry
// would erroneously 409 with "registry full".
func TestKVTrustedKeyStore_RegisterUpsertDoesNotConsumeCapSlot(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	store, err := auth.NewKVTrustedKeyStore(ctx, kvStore, auth.WithMaxTrustedKeys(2))
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}

	for i, kid := range []string{"cap-a", "cap-b"} {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		tk := &auth.TrustedKey{
			KID:       kid,
			TenantID:  spi.SystemTenantID,
			PublicKey: &key.PublicKey,
			Audience:  "svc",
			Active:    true,
			ValidFrom: time.Now().UTC(),
		}
		if err := store.Register(tk, auth.RotateOptions{}); err != nil {
			t.Fatalf("Register %d (%s): %v", i, kid, err)
		}
	}

	// Registry now at cap (2/2). An upsert on cap-a must succeed.
	rotatedKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rotated := &auth.TrustedKey{
		KID:       "cap-a",
		TenantID:  spi.SystemTenantID,
		PublicKey: &rotatedKey.PublicKey,
		Audience:  "rotated",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}
	if err := store.Register(rotated, auth.RotateOptions{}); err != nil {
		t.Fatalf("upsert on cap-saturated registry: expected nil, got %v", err)
	}

	// Inserting a *new* KID (cap-c) must still be rejected — registry full.
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	novel := &auth.TrustedKey{
		KID:       "cap-c",
		TenantID:  spi.SystemTenantID,
		PublicKey: &newKey.PublicKey,
		Audience:  "svc",
		Active:    true,
		ValidFrom: time.Now().UTC(),
	}
	err = store.Register(novel, auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected Register of new KID at cap to be rejected, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
}

func TestKVTrustedKeyStore_ListPersists(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := systemCtx()
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	key2, _ := rsa.GenerateKey(rand.Reader, 2048)

	store1, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore: %v", err)
	}
	if err := store1.Register(&auth.TrustedKey{
		KID: "list-1", TenantID: spi.SystemTenantID, PublicKey: &key1.PublicKey, Audience: "a", Active: true, ValidFrom: time.Now().UTC(),
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register list-1: %v", err)
	}
	if err := store1.Register(&auth.TrustedKey{
		KID: "list-2", TenantID: spi.SystemTenantID, PublicKey: &key2.PublicKey, Audience: "b", Active: true, ValidFrom: time.Now().UTC(),
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register list-2: %v", err)
	}

	// New instance should list both.
	store2, err := auth.NewKVTrustedKeyStore(ctx, kvStore)
	if err != nil {
		t.Fatalf("NewKVTrustedKeyStore (instance 2): %v", err)
	}
	all := store2.List(spi.SystemTenantID)
	if len(all) != 2 {
		t.Errorf("expected 2 keys on new instance, got %d", len(all))
	}
}

func TestKVTrustedKeyStore_TenantScopedKeyEncoding(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	store, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tID := spi.TenantID("tenant-a")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tID, PublicKey: &priv.PublicKey, JWK: map[string]any{"kty": "RSA", "kid": "k1"}, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := store.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	all, err := kv.List(ctx, "trusted-keys")
	if err != nil {
		t.Fatalf("kv list: %v", err)
	}
	if _, ok := all["tenant-a:k1"]; !ok {
		t.Errorf("expected key 'tenant-a:k1' in KV; keys: %v", mapKeys(all))
	}
}

func TestKVTrustedKeyStore_NoCrossTenantCachePollution(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	store, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tA, PublicKey: &priv.PublicKey, JWK: map[string]any{"kty": "RSA", "kid": "k1"}, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = store.Register(tk, auth.RotateOptions{})
	if _, err := store.Get(tA, "k1"); err != nil {
		t.Fatalf("A.Get: %v", err)
	}
	if _, err := store.Get(tB, "k1"); err == nil {
		t.Error("B.Get(k1) leaked A's key")
	}
	if _, err := store.Get(tA, "k1"); err != nil {
		t.Errorf("A.Get post-B failure: %v", err)
	}
}

func TestKVTrustedKeyStore_RoundTripsTenantIDAndJWK(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tID := spi.TenantID("t1")
	originalJWK := map[string]any{"kty": "RSA", "kid": "k", "extra": "field"}
	tk := &auth.TrustedKey{KID: "k", TenantID: tID, PublicKey: &priv.PublicKey, JWK: originalJWK, Audience: "human", Active: true, ValidFrom: time.Now()}
	store, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	_ = store.Register(tk, auth.RotateOptions{})
	store2, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := store2.Get(tID, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TenantID != tID {
		t.Errorf("TenantID lost: %q", got.TenantID)
	}
	if got.JWK["extra"] != "field" {
		t.Errorf("JWK 'extra' lost: %+v", got.JWK)
	}
}

func mapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustNewMemoryKV(t *testing.T, ctx context.Context) spi.KeyValueStore {
	t.Helper()
	factory := memory.NewStoreFactory()
	kv, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("memory KV: %v", err)
	}
	return kv
}
