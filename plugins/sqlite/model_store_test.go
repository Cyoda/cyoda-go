package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func setupModelStore(t *testing.T) (spi.ModelStore, context.Context) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "model_store_test.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx := extTestCtx("model-tenant")
	store, err := f.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	return store, ctx
}

func makeModelDesc(name, version string) *spi.ModelDescriptor {
	return &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: name, ModelVersion: version},
		State:       spi.ModelUnlocked,
		ChangeLevel: spi.ChangeLevelArrayElements,
		UpdateDate:  time.Now().UTC().Truncate(time.Millisecond),
		Schema:      []byte(`{"type":"object"}`),
	}
}

// TestModelStore_SQLite_UniqueKeysRoundTrip verifies that UniqueKeys survive a Save/Get cycle.
func TestModelStore_SQLite_UniqueKeysRoundTrip(t *testing.T) {
	store, ctx := setupModelStore(t)

	desc := makeModelDesc("Widget", "1")
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
		t.Errorf("UniqueKeys IDs mismatch: got %v", got.UniqueKeys)
	}
	if len(got.UniqueKeys[0].Fields) != 2 || got.UniqueKeys[0].Fields[0] != "tenantId" {
		t.Errorf("UniqueKeys[0].Fields mismatch: got %v", got.UniqueKeys[0].Fields)
	}
}

// TestModelStore_SQLite_UniqueKeysLockPreservation verifies that the sqlite Lock
// read-modify-write does NOT strip UniqueKeys (#9 strip hazard).
// Sequence: Save(UniqueKeys) → Lock → Get → assert UniqueKeys intact.
func TestModelStore_SQLite_UniqueKeysLockPreservation(t *testing.T) {
	store, ctx := setupModelStore(t)

	desc := makeModelDesc("Order", "1")
	desc.UniqueKeys = []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"tenantId", "orderRef"}},
	}

	if err := store.Save(ctx, desc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Lock triggers the read-modify-write path that is the strip hazard.
	if err := store.Lock(ctx, desc.Ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	got, err := store.Get(ctx, desc.Ref)
	if err != nil {
		t.Fatalf("Get after Lock: %v", err)
	}
	if len(got.UniqueKeys) != 1 {
		t.Fatalf("UniqueKeys stripped by Lock: got %d keys, want 1", len(got.UniqueKeys))
	}
	if got.UniqueKeys[0].ID != "uk1" {
		t.Errorf("UniqueKeys[0].ID after Lock: got %q, want %q", got.UniqueKeys[0].ID, "uk1")
	}
	if len(got.UniqueKeys[0].Fields) != 2 || got.UniqueKeys[0].Fields[0] != "tenantId" {
		t.Errorf("UniqueKeys[0].Fields after Lock: got %v", got.UniqueKeys[0].Fields)
	}
	// Confirm it's actually locked.
	if got.State != spi.ModelLocked {
		t.Errorf("expected LOCKED state, got %v", got.State)
	}
}
