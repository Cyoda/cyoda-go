package postgres_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/plugins/postgres"
)

// TestPostgres_TombstoneClear_EarlierVersionReadUnchanged: clearing the
// transition on the DELETED tombstone must not touch the version it was saved
// with. A point-in-time read before the delete still reports "submit", the
// current-state read after the delete finds nothing, and the metadata lists
// the transition on the saved version only.
func TestPostgres_TombstoneClear_EarlierVersionReadUnchanged(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	factory := postgres.NewStoreFactory(pool)
	const tenant spi.TenantID = "tenant-tombstone-transition"
	ctx := ctxWithTenant(tenant)
	ref := spi.ModelRef{EntityName: "m-tombstone-transition", ModelVersion: "1"}
	lockModels(t, factory, ctx, ref)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	const id = "e-tombstone-transition"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: ref, TransitionForLatestSave: "submit"},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d versions, want 2: %+v", len(metas), metas)
	}
	// Newest first: tombstone, then the saved version.
	if !metas[0].Deleted || metas[0].Transition != "" {
		t.Errorf("tombstone = %+v, want Deleted with empty Transition", metas[0])
	}
	if metas[1].Transition != "submit" {
		t.Errorf("saved version Transition = %q, want %q", metas[1].Transition, "submit")
	}

	// The saved version's own instant, which precedes the delete.
	got, err := store.GetAsAt(ctx, id, metas[1].Timestamp)
	if err != nil {
		t.Fatalf("GetAsAt before delete: %v", err)
	}
	if got.Meta.TransitionForLatestSave != "submit" {
		t.Errorf("GetAsAt TransitionForLatestSave = %q, want %q", got.Meta.TransitionForLatestSave, "submit")
	}

	if _, err := store.Get(ctx, id); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want spi.ErrNotFound", err)
	}
}
