package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
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
	ctx := systemCtx()
	kvStore := mustNewMemoryKV(t, ctx)
	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	expiry := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	tk := &auth.TrustedKey{
		KID:       "persist-key-1",
		TenantID:  spi.SystemTenantID,
		PublicKey: &key1.PublicKey,
		Issuers:   []string{"https://issuer.example.com", "https://backup-issuer.example.com"},
		Active:    true,
		ValidFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:   &expiry,
	}

	store1 := auth.NewKVTrustedKeyStore(kvStore, 0)
	if err := store1.Register(ctx, tk, false); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A second instance over the same KV store (a restart, or another node).
	store2 := auth.NewKVTrustedKeyStore(kvStore, 0)
	got2, err := store2.Get(ctx, spi.SystemTenantID, "persist-key-1")
	if err != nil {
		t.Fatalf("Get on instance 2: %v", err)
	}
	if got2.KID != "persist-key-1" || !got2.Active {
		t.Errorf("unexpected key: KID=%s Active=%v", got2.KID, got2.Active)
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
	if got2.PublicKey == nil || got2.PublicKey.N.Cmp(key1.PublicKey.N) != 0 || got2.PublicKey.E != key1.PublicKey.E {
		t.Error("public key mismatch after round-trip")
	}
}

// A key registered through one node's store verifies through another's at
// once: there is no node copy to bring up to date.
func TestKVTrustedKeyStore_RegisterVisibleOnEveryNodeAtOnce(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	a, b := auth.NewKVTrustedKeyStore(kv, 0), auth.NewKVTrustedKeyStore(kv, 0)
	if err := a.Register(systemCtx(), newTrustedKey(t, "acme", "k1", time.Now().Add(-time.Minute)), false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetForVerification(systemCtx(), "acme", "k1"); err != nil {
		t.Fatalf("node b: %v", err)
	}
}

func TestKVTrustedKeyStore_DeletePersists(t *testing.T) {
	ctx := systemCtx()
	kvStore := mustNewMemoryKV(t, ctx)
	store1 := auth.NewKVTrustedKeyStore(kvStore, 0)
	if err := store1.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "del-key", time.Now()), false); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store1.Delete(ctx, spi.SystemTenantID, "del-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	store2 := auth.NewKVTrustedKeyStore(kvStore, 0)
	if _, err := store2.Get(ctx, spi.SystemTenantID, "del-key"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("deleted key on instance 2: err = %v, want ErrTrustedKeyNotFound", err)
	}
}

func TestKVTrustedKeyStore_InvalidateReactivatePersists(t *testing.T) {
	ctx := systemCtx()
	kvStore := mustNewMemoryKV(t, ctx)
	store1 := auth.NewKVTrustedKeyStore(kvStore, 0)
	if err := store1.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "toggle-key", time.Now()), false); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store1.Invalidate(ctx, spi.SystemTenantID, "toggle-key"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	store2 := auth.NewKVTrustedKeyStore(kvStore, 0)
	got, err := store2.Get(ctx, spi.SystemTenantID, "toggle-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Active {
		t.Error("expected key to be inactive after persist")
	}

	future := time.Now().Add(24 * time.Hour)
	if err := store2.Reactivate(ctx, spi.SystemTenantID, "toggle-key", time.Now(), future); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	got3, err := auth.NewKVTrustedKeyStore(kvStore, 0).Get(ctx, spi.SystemTenantID, "toggle-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got3.Active {
		t.Error("expected key to be active after reactivate persist")
	}
}

// TestKVTrustedKeyStore_RegisterRespectsMaxTrustedKeys verifies that the store
// rejects Register once the configured cap is reached.
func TestKVTrustedKeyStore_RegisterRespectsMaxTrustedKeys(t *testing.T) {
	ctx := systemCtx()
	store := auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, ctx), 3)
	for i := 0; i < 3; i++ {
		if err := store.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "cap-key-"+string(rune('a'+i)), time.Now()), false); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}
	err := store.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "cap-key-overflow", time.Now()), false)
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("status=%d code=%q, want 400 %s", appErr.Status, appErr.Code, common.ErrCodeTrustedKeyCapReached)
	}
}

// A rotation (invalidatePrevious) ends every other key of the tenant, so the
// tenant holds one verifying key afterwards: the cap never refuses it.
func TestKVTrustedKeyStore_RotationAtCapIsNotRefused(t *testing.T) {
	ctx := systemCtx()
	store := auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, ctx), 2)
	for _, kid := range []string{"a", "b"} {
		if err := store.Register(ctx, newTrustedKey(t, "acme", kid, time.Now().Add(-time.Minute)), false); err != nil {
			t.Fatalf("register %s: %v", kid, err)
		}
	}
	if err := store.Register(ctx, newTrustedKey(t, "acme", "c", time.Now().Add(-time.Minute)), true); err != nil {
		t.Fatalf("rotation at the cap: %v", err)
	}
	for _, kid := range []string{"a", "b"} {
		if _, err := store.GetForVerification(ctx, "acme", kid); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Errorf("%s after rotation: err = %v, want ErrTrustedKeyNotFound", kid, err)
		}
	}
	if _, err := store.GetForVerification(ctx, "acme", "c"); err != nil {
		t.Fatalf("rotated-in key: %v", err)
	}
}

// TestKVTrustedKeyStore_RegisterUpsertsSameKID pins the upsert contract:
// same tenant + same KID replaces the record, so a retried registration
// succeeds.
func TestKVTrustedKeyStore_RegisterUpsertsSameKID(t *testing.T) {
	ctx := systemCtx()
	kvStore := mustNewMemoryKV(t, ctx)
	store := auth.NewKVTrustedKeyStore(kvStore, 0)
	if err := store.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "rotate-kid", time.Now()), false); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	rotated := newTrustedKey(t, spi.SystemTenantID, "rotate-kid", time.Now())
	rotated.Issuers = []string{"https://rotated.example.com"}
	if err := store.Register(ctx, rotated, false); err != nil {
		t.Fatalf("re-Register (upsert): %v", err)
	}
	got, err := auth.NewKVTrustedKeyStore(kvStore, 0).Get(ctx, spi.SystemTenantID, "rotate-kid")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PublicKey.N.Cmp(rotated.PublicKey.N) != 0 || len(got.Issuers) != 1 || got.Issuers[0] != "https://rotated.example.com" {
		t.Errorf("upsert did not replace the record: %+v", got)
	}
}

// TestKVTrustedKeyStore_RegisterUpsertDoesNotConsumeCapSlot: an upsert on an
// existing KID is not blocked by a full cap, because it does not grow the
// tenant's keys; a new KID still is.
func TestKVTrustedKeyStore_RegisterUpsertDoesNotConsumeCapSlot(t *testing.T) {
	ctx := systemCtx()
	store := auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, ctx), 2)
	for _, kid := range []string{"cap-a", "cap-b"} {
		if err := store.Register(ctx, newTrustedKey(t, spi.SystemTenantID, kid, time.Now()), false); err != nil {
			t.Fatalf("Register %s: %v", kid, err)
		}
	}
	if err := store.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "cap-a", time.Now()), false); err != nil {
		t.Fatalf("upsert at the cap: %v", err)
	}
	err := store.Register(ctx, newTrustedKey(t, spi.SystemTenantID, "cap-c", time.Now()), false)
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest {
		t.Fatalf("new KID at the cap: err = %v, want a 400 AppError", err)
	}
}

func TestKVTrustedKeyStore_ListPersists(t *testing.T) {
	ctx := systemCtx()
	kvStore := mustNewMemoryKV(t, ctx)
	store1 := auth.NewKVTrustedKeyStore(kvStore, 0)
	for _, kid := range []string{"list-2", "list-1"} {
		if err := store1.Register(ctx, newTrustedKey(t, spi.SystemTenantID, kid, time.Now()), false); err != nil {
			t.Fatalf("Register %s: %v", kid, err)
		}
	}
	all, err := auth.NewKVTrustedKeyStore(kvStore, 0).List(ctx, spi.SystemTenantID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].KID != "list-1" || all[1].KID != "list-2" {
		t.Errorf("List = %v, want list-1, list-2 in kid order", all)
	}
}

// The storage layout: one namespace per tenant ("trusted-keys:<tenant>"),
// keyed by kid, and no audience in the record.
func TestKVTrustedKeyStore_PerTenantNamespaceLayout(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	store := auth.NewKVTrustedKeyStore(kv, 0)
	if err := store.Register(ctx, newTrustedKey(t, "tenant-a", "k1", time.Now()), false); err != nil {
		t.Fatalf("register: %v", err)
	}
	all, err := kv.List(ctx, "trusted-keys:tenant-a")
	if err != nil {
		t.Fatalf("kv list: %v", err)
	}
	data, ok := all["k1"]
	if !ok || len(all) != 1 {
		t.Fatalf("namespace trusted-keys:tenant-a holds %v, want exactly k1", mapKeys(all))
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if _, has := rec["audience"]; has {
		t.Errorf("record carries an audience: %s", data)
	}
	if rec["tenantID"] != "tenant-a" || rec["kid"] != "k1" {
		t.Errorf("record = %s", data)
	}
}

// A record is bound to the namespace and KV key it is stored under: a
// record copied into another tenant's namespace is not that tenant's key.
// It does not decode — a store error, never a key that verifies.
func TestKVTrustedKeyStore_RecordBoundToItsNamespace(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	store := auth.NewKVTrustedKeyStore(kv, 0)
	if err := store.Register(ctx, newTrustedKey(t, "tenant-a", "k1", time.Now().Add(-time.Minute)), false); err != nil {
		t.Fatal(err)
	}
	data, err := kv.Get(ctx, "trusted-keys:tenant-a", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, "trusted-keys:tenant-b", "k1", data); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, "trusted-keys:tenant-a", "k2", data); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tenant spi.TenantID
		kid    string
	}{{"tenant-b", "k1"}, {"tenant-a", "k2"}} {
		if _, err := store.GetForVerification(ctx, c.tenant, c.kid); err == nil || errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Errorf("%s/%s: err = %v, want a decode error", c.tenant, c.kid, err)
		}
	}
	list, err := store.List(ctx, "tenant-b")
	if err != nil || len(list) != 0 {
		t.Errorf("tenant-b list = %v, %v; want empty", list, err)
	}
}

func TestKVTrustedKeyStore_RoundTripsTenantIDAndJWK(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	tk := newTrustedKey(t, "t1", "k", time.Now())
	tk.JWK = map[string]any{"kty": "RSA", "kid": "k", "extra": "field"}
	if err := auth.NewKVTrustedKeyStore(kv, 0).Register(ctx, tk, false); err != nil {
		t.Fatal(err)
	}
	got, err := auth.NewKVTrustedKeyStore(kv, 0).Get(ctx, "t1", "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TenantID != "t1" {
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

// newTestTrustedStore is a KVTrustedKeyStore with no cap over a fresh
// in-memory KV store.
func newTestTrustedStore(t *testing.T) *auth.KVTrustedKeyStore {
	t.Helper()
	return auth.NewKVTrustedKeyStore(mustNewMemoryKV(t, systemCtx()), 0)
}

// newTestAuthService builds an AuthService over a fresh in-memory KV store;
// cfg.KV is filled in.
func newTestAuthService(t *testing.T, cfg auth.AuthConfig) *auth.AuthService {
	t.Helper()
	cfg.KV = mustNewMemoryKV(t, systemCtx())
	svc, err := auth.NewAuthService(systemCtx(), cfg)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	return svc
}

// failingKV fails a Put of KV key failOn, in any namespace.
type failingKV struct {
	spi.KeyValueStore
	failOn string
}

func (f *failingKV) Put(ctx context.Context, ns, key string, value []byte) error {
	if key == f.failOn {
		return fmt.Errorf("injected failure")
	}
	return f.KeyValueStore.Put(ctx, ns, key, value)
}

// A rotation writes the previous keys first and the new key last: when the
// new key's write fails, the new key is absent and the previous key is
// already ended — fail closed; a retry completes the rotation.
func TestKVTrustedKeyStore_RotationNewKeyWriteFailureFailsClosed(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	tID := spi.TenantID("t")
	if err := auth.NewKVTrustedKeyStore(mem, 0).Register(ctx, newTrustedKey(t, tID, "a", time.Now().Add(-time.Minute)), false); err != nil {
		t.Fatal(err)
	}
	store := auth.NewKVTrustedKeyStore(&failingKV{KeyValueStore: mem, failOn: "b"}, 0)
	b := newTrustedKey(t, tID, "b", time.Now().Add(-time.Minute))
	if err := store.Register(ctx, b, true); err == nil {
		t.Fatal("expected error from new-key KV write failure")
	}
	if _, err := store.Get(ctx, tID, "b"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("new key b after its own write failed: err = %v, want ErrTrustedKeyNotFound", err)
	}
	if _, err := store.GetForVerification(ctx, tID, "a"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Errorf("previous key a still verifies: err = %v", err)
	}
	retry := auth.NewKVTrustedKeyStore(mem, 0)
	if err := retry.Register(ctx, b, true); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := retry.GetForVerification(ctx, tID, "b"); err != nil {
		t.Fatalf("b after retry: %v", err)
	}
}

// TestKVTrustedKeyStore_Reactivate_RejectsZeroValidTo verifies that
// KVTrustedKeyStore.Reactivate enforces its window contract itself: validTo
// is required and must be in the future and after validFrom, so a caller
// bypassing the adapter cannot accidentally create an immortal key.
func TestKVTrustedKeyStore_Reactivate_RejectsZeroValidTo(t *testing.T) {
	ctx := systemCtx()
	store := newTestTrustedStore(t)
	tID := spi.TenantID("t1")
	past := time.Now().Add(-1 * time.Hour)
	tk := newTrustedKey(t, tID, "k", past)
	tk.Active, tk.ValidTo = false, &past
	if err := store.Register(ctx, tk, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Reactivate(ctx, tID, "k", time.Now(), time.Time{}); err == nil {
		t.Error("expected error for zero validTo")
	}
	if err := store.Reactivate(ctx, tID, "k", past.Add(-1*time.Hour), past); err == nil {
		t.Error("expected error for past validTo")
	}
	future := time.Now().Add(24 * time.Hour)
	wayFuture := time.Now().Add(48 * time.Hour)
	if err := store.Reactivate(ctx, tID, "k", wayFuture, future); err == nil {
		t.Error("expected error for validTo < validFrom")
	}
	if err := store.Reactivate(ctx, tID, "k", time.Now(), future); err != nil {
		t.Errorf("valid Reactivate failed: %v", err)
	}
}

func TestKVTrustedKeyStore_SameKidInTwoTenants(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVTrustedKeyStore(kv, 10)
	ka, kb := generateTestKey(t), generateTestKey(t)
	for tenant, key := range map[spi.TenantID]*rsa.PrivateKey{"acme": ka, "beta": kb} {
		if err := s.Register(systemCtx(), &auth.TrustedKey{KID: "k1", TenantID: tenant,
			PublicKey: &key.PublicKey, Active: true, ValidFrom: time.Now().Add(-time.Minute)}, false); err != nil {
			t.Fatalf("register in %s: %v", tenant, err)
		}
	}
	got, err := s.GetForVerification(systemCtx(), "acme", "k1")
	if err != nil || got.PublicKey.N.Cmp(ka.PublicKey.N) != 0 {
		t.Fatalf("acme k1 = %v, %v; want acme's key", got, err)
	}
	list, err := s.List(systemCtx(), "beta")
	if err != nil || len(list) != 1 || list[0].PublicKey.N.Cmp(kb.PublicKey.N) != 0 {
		t.Fatalf("beta list = %v, %v", list, err)
	}
}

func TestKVTrustedKeyStore_InvalidateEndsAtOnce(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVTrustedKeyStore(kv, 10)
	key := generateTestKey(t)
	_ = s.Register(systemCtx(), &auth.TrustedKey{KID: "k1", TenantID: "acme", PublicKey: &key.PublicKey,
		Active: true, ValidFrom: time.Now().Add(-time.Minute)}, false)
	if err := s.Invalidate(systemCtx(), "acme", "k1"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := s.GetForVerification(systemCtx(), "acme", "k1"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("after invalidate: %v, want ErrTrustedKeyNotFound", err)
	}
	// A second store instance over the same KV (another node) agrees at once.
	other := auth.NewKVTrustedKeyStore(kv, 10)
	if _, err := other.GetForVerification(systemCtx(), "acme", "k1"); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
		t.Fatalf("other node after invalidate: %v", err)
	}
}

func TestKVTrustedKeyStore_ListKeepsUnavailableMarker(t *testing.T) {
	s := auth.NewKVTrustedKeyStore(brokenKV{}, 10) // the storage-unavailable fake in kv_m2m_store_test.go
	_, err := s.List(systemCtx(), "acme")
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("List error lost its storage-unavailable marker: %v", err)
	}
}

// The exchange's read keeps the storage-unavailable marker too, and is never
// read as "no such key".
func TestKVTrustedKeyStore_GetForVerificationKeepsUnavailableMarker(t *testing.T) {
	s := auth.NewKVTrustedKeyStore(brokenKV{}, 10)
	_, err := s.GetForVerification(systemCtx(), "acme", "k1")
	var su interface{ StorageUnavailable() bool }
	if errors.Is(err, auth.ErrTrustedKeyNotFound) || !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("err = %v, want a storage-unavailable store error", err)
	}
}

// A kid outside the trusted-key grammar comes from an untrusted subject
// token header: it is refused as not found without reaching the store.
func TestKVTrustedKeyStore_GetForVerificationRefusesMalformedKidWithoutReading(t *testing.T) {
	s := auth.NewKVTrustedKeyStore(brokenKV{}, 10) // any read would error
	for _, kid := range []string{"", "a b", "a:b", "a/b", "\x00", string(make([]byte, 129))} {
		if _, err := s.GetForVerification(systemCtx(), "acme", kid); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Errorf("%q: err = %v, want ErrTrustedKeyNotFound without a store read", kid, err)
		}
	}
}
