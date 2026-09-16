package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// readbackInstant reads the entity's own backend-stamped time rather than a
// client clock — the point-in-time semantics spec requires black-box
// exact-T tests to query at the backend-reported timestamp.
func readbackInstant(t *testing.T, ctx context.Context, store spi.EntityStore, id string) time.Time {
	t.Helper()
	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{Limit: 1})
	if err != nil {
		t.Fatalf("GetVersionMetadata: %v", err)
	}
	if len(metas) == 0 {
		t.Fatalf("no version metadata for %s", id)
	}
	return metas[0].Timestamp
}

// TestPIT_DeletedSinceInstant_StillReturned pins the case the lateral form
// must not lose: an entity deleted AFTER the instant is still part of the
// snapshot at that instant.
func TestPIT_DeletedSinceInstant_StillReturned(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "pit-lateral", ModelVersion: "1"}

	id := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref, State: "LIVE"},
		Data: []byte(`{"category":"physics"}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	instant := readbackInstant(t, ctx, store, id) // the backend-stamped time of that version

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at instant: %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != id {
		t.Fatalf("entity deleted after the instant must still appear in the snapshot at it: got %d results", len(got))
	}

	now := time.Now()
	after, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &now,
	})
	if err != nil {
		t.Fatalf("Search now: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("deleted entity must be absent at now: got %d results", len(after))
	}
}

// TestPIT_CreatedAfterInstant_Absent pins the other direction.
func TestPIT_CreatedAfterInstant_Absent(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("entity-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "pit-lateral-after", ModelVersion: "1"}

	seed := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: seed, ModelRef: mref}, Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	instant := readbackInstant(t, ctx, store, seed)

	later := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: later, ModelRef: mref}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("later Save: %v", err)
	}

	got, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at instant: %v", err)
	}
	for _, e := range got {
		if e.Meta.ID == later {
			t.Fatal("an entity created after the instant must not appear in the snapshot at it")
		}
	}
}
