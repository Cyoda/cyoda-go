package postgres_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func setupModelTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil { t.Fatalf("reset schema: %v", err) }
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func makeDescriptor(name, version string) *spi.ModelDescriptor {
	return &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: name, ModelVersion: version},
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelArrayElements,
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
		Schema:      []byte(`{"type":"object","properties":{"id":{"type":"string"}}}`),
	}
}

func TestModelStore_SaveAndGet(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")

	store, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}

	desc := makeDescriptor("Widget", "1")
	if err := store.Save(ctx, desc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Ref != desc.Ref {
		t.Errorf("Ref: got %v, want %v", got.Ref, desc.Ref)
	}
	if got.State != desc.State {
		t.Errorf("State: got %v, want %v", got.State, desc.State)
	}
	if got.ChangeLevel != desc.ChangeLevel {
		t.Errorf("ChangeLevel: got %v, want %v", got.ChangeLevel, desc.ChangeLevel)
	}
	if string(got.Schema) != string(desc.Schema) {
		t.Errorf("Schema: got %q, want %q", got.Schema, desc.Schema)
	}
}

func TestModelStore_SaveOverwrite(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	desc := makeDescriptor("Widget", "1")
	store.Save(ctx, desc)

	desc2 := *desc
	desc2.State = spi.ModelLocked
	desc2.ChangeLevel = spi.ChangeLevelStructural
	if err := store.Save(ctx, &desc2); err != nil {
		t.Fatalf("Save overwrite: %v", err)
	}

	got, err := store.Get(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	if got.State != spi.ModelLocked {
		t.Errorf("State after overwrite: got %v, want %v", got.State, spi.ModelLocked)
	}
	if got.ChangeLevel != spi.ChangeLevelStructural {
		t.Errorf("ChangeLevel after overwrite: got %v, want %v", got.ChangeLevel, spi.ChangeLevelStructural)
	}
}

func TestModelStore_GetNotFound(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	_, err := store.Get(ctx, spi.ModelRef{EntityName: "NoSuch", ModelVersion: "1"})
	if err == nil {
		t.Fatal("expected error for nonexistent model")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestModelStore_GetAll(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	store.Save(ctx, makeDescriptor("Alpha", "1"))
	store.Save(ctx, makeDescriptor("Beta", "2"))

	refs, err := store.GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}

	refSet := make(map[string]bool)
	for _, r := range refs {
		refSet[r.EntityName+"."+r.ModelVersion] = true
	}
	if !refSet["Alpha.1"] {
		t.Error("missing Alpha.1")
	}
	if !refSet["Beta.2"] {
		t.Error("missing Beta.2")
	}
}

func TestModelStore_GetAllEmpty(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	refs, err := store.GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll empty: %v", err)
	}
	if refs == nil {
		t.Fatal("expected empty slice, got nil")
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs, got %d", len(refs))
	}
}

func TestModelStore_Delete(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	desc := makeDescriptor("Widget", "1")
	store.Save(ctx, desc)

	if err := store.Delete(ctx, desc.Ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := store.Get(ctx, desc.Ref)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestModelStore_DeleteNonexistent(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	// SPI contract: deleting a nonexistent model must return ErrNotFound.
	err := store.Delete(ctx, spi.ModelRef{EntityName: "NoSuch", ModelVersion: "1"})
	if err == nil {
		t.Fatal("expected ErrNotFound when deleting a nonexistent model, got nil")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestModelStore_LockUnlock(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	desc := makeDescriptor("Widget", "1")
	store.Save(ctx, desc)

	// Initially unlocked
	locked, err := store.IsLocked(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if locked {
		t.Error("expected unlocked after save")
	}

	// Lock it
	if err := store.Lock(ctx, desc.Ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	locked, err = store.IsLocked(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("IsLocked after lock: %v", err)
	}
	if !locked {
		t.Error("expected locked after Lock()")
	}

	// Unlock it
	if err := store.Unlock(ctx, desc.Ref); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	locked, err = store.IsLocked(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("IsLocked after unlock: %v", err)
	}
	if locked {
		t.Error("expected unlocked after Unlock()")
	}
}

func TestModelStore_LockNotFound(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	err := store.Lock(ctx, spi.ModelRef{EntityName: "NoSuch", ModelVersion: "1"})
	if err == nil {
		t.Fatal("expected error locking nonexistent model")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestModelStore_UnlockNotFound(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	err := store.Unlock(ctx, spi.ModelRef{EntityName: "NoSuch", ModelVersion: "1"})
	if err == nil {
		t.Fatal("expected error unlocking nonexistent model")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestModelStore_IsLockedNotFound(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	_, err := store.IsLocked(ctx, spi.ModelRef{EntityName: "NoSuch", ModelVersion: "1"})
	if err == nil {
		t.Fatal("expected error for IsLocked on nonexistent model")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestModelStore_SetChangeLevel(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	desc := makeDescriptor("Widget", "1")
	store.Save(ctx, desc)

	if err := store.SetChangeLevel(ctx, desc.Ref, spi.ChangeLevelStructural); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}

	got, err := store.Get(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("Get after SetChangeLevel: %v", err)
	}
	if got.ChangeLevel != spi.ChangeLevelStructural {
		t.Errorf("ChangeLevel: got %v, want %v", got.ChangeLevel, spi.ChangeLevelStructural)
	}
}

func TestModelStore_SetChangeLevelNotFound(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, _ := factory.ModelStore(ctx)

	err := store.SetChangeLevel(ctx, spi.ModelRef{EntityName: "NoSuch", ModelVersion: "1"}, spi.ChangeLevelStructural)
	if err == nil {
		t.Fatal("expected error for SetChangeLevel on nonexistent model")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestModelStore_UniqueKeysRoundTrip verifies that UniqueKeys survive a Save/Get cycle.
func TestModelStore_UniqueKeysRoundTrip(t *testing.T) {
	factory := setupModelTest(t)
	ctx := ctxWithTenant("model-tenant")
	store, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}

	desc := makeDescriptor("Widget", "1")
	desc.UniqueKeys = []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"tenantId", "orderNumber"}},
		{ID: "uk2", Fields: []string{"externalRef"}},
	}
	if err := store.Save(ctx, desc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.UniqueKeys) != 2 {
		t.Fatalf("UniqueKeys length: got %d, want 2", len(got.UniqueKeys))
	}
	if got.UniqueKeys[0].ID != "uk1" || got.UniqueKeys[1].ID != "uk2" {
		t.Errorf("UniqueKeys mismatch: got %v", got.UniqueKeys)
	}
	if len(got.UniqueKeys[0].Fields) != 2 || got.UniqueKeys[0].Fields[0] != "tenantId" {
		t.Errorf("UniqueKeys[0].Fields mismatch: got %v", got.UniqueKeys[0].Fields)
	}
}

func TestModelStore_TenantIsolation(t *testing.T) {
	factory := setupModelTest(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, _ := factory.ModelStore(ctxA)
	storeB, _ := factory.ModelStore(ctxB)

	desc := makeDescriptor("SharedName", "1")
	storeA.Save(ctxA, desc)

	// Tenant B cannot see tenant A's model
	_, err := storeB.Get(ctxB, desc.Ref)
	if err == nil {
		t.Fatal("tenant-B should not see tenant-A's model")
	}

	// Tenant B GetAll should be empty
	refs, err := storeB.GetAll(ctxB)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("tenant-B should see 0 refs, got %d", len(refs))
	}

	// Tenant A still sees it
	refs, err = storeA.GetAll(ctxA)
	if err != nil {
		t.Fatalf("GetAll tenant-A: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("tenant-A should see 1 ref, got %d", len(refs))
	}
}
