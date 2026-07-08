package postgres_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func setupKVTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func TestKVStore_PutAndGet(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")

	store, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}

	if err := store.Put(ctx, "ns1", "key1", []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, "ns1", "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("expected 'hello', got %q", string(got))
	}
}

func TestKVStore_PutOverwrite(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	store.Put(ctx, "ns1", "key1", []byte("v1"))
	store.Put(ctx, "ns1", "key1", []byte("v2"))

	got, err := store.Get(ctx, "ns1", "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("expected 'v2' after overwrite, got %q", string(got))
	}
}

func TestKVStore_GetNotFound(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	_, err := store.Get(ctx, "ns1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
}

func TestKVStore_Delete(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	store.Put(ctx, "ns1", "key1", []byte("hello"))

	if err := store.Delete(ctx, "ns1", "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := store.Get(ctx, "ns1", "key1")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestKVStore_DeleteNonexistent(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	if err := store.Delete(ctx, "ns1", "nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent should not error, got: %v", err)
	}
}

func TestKVStore_List(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	store.Put(ctx, "ns1", "a", []byte("1"))
	store.Put(ctx, "ns1", "b", []byte("2"))
	store.Put(ctx, "ns1", "c", []byte("3"))

	result, err := store.List(ctx, "ns1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	if string(result["a"]) != "1" || string(result["b"]) != "2" || string(result["c"]) != "3" {
		t.Errorf("unexpected values: %v", result)
	}
}

func TestKVStore_ListEmpty(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	result, err := store.List(ctx, "empty-ns")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result == nil {
		t.Fatal("expected empty map, got nil")
	}
	if len(result) != 0 {
		t.Errorf("expected 0 entries, got %d", len(result))
	}
}

func TestKVStore_TenantIsolation(t *testing.T) {
	factory := setupKVTest(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, _ := factory.KeyValueStore(ctxA)
	storeB, _ := factory.KeyValueStore(ctxB)

	storeA.Put(ctxA, "ns1", "secret", []byte("tenant-A-data"))

	// Tenant B should not see tenant A's data
	_, err := storeB.Get(ctxB, "ns1", "secret")
	if err == nil {
		t.Fatal("tenant-B should not see tenant-A's key")
	}

	// Tenant B list should be empty
	result, _ := storeB.List(ctxB, "ns1")
	if len(result) != 0 {
		t.Errorf("tenant-B should see 0 entries, got %d", len(result))
	}
}

func TestKVStore_NamespaceIsolation(t *testing.T) {
	factory := setupKVTest(t)
	ctx := ctxWithTenant("kv-tenant")
	store, _ := factory.KeyValueStore(ctx)

	store.Put(ctx, "ns1", "key1", []byte("from-ns1"))
	store.Put(ctx, "ns2", "key1", []byte("from-ns2"))

	got1, _ := store.Get(ctx, "ns1", "key1")
	got2, _ := store.Get(ctx, "ns2", "key1")

	if string(got1) != "from-ns1" {
		t.Errorf("ns1: expected 'from-ns1', got %q", string(got1))
	}
	if string(got2) != "from-ns2" {
		t.Errorf("ns2: expected 'from-ns2', got %q", string(got2))
	}
}
