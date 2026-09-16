package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// readbackInstant reads the entity's own backend-stamped time rather than a
// client clock — the point-in-time semantics spec requires black-box
// exact-T tests to query at the backend-reported timestamp.
func readbackInstant(ctx context.Context, t *testing.T, store spi.EntityStore, id string) time.Time {
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

	instant := readbackInstant(ctx, t, store, id) // the backend-stamped time of that version

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
	instant := readbackInstant(ctx, t, store, seed)

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
	// Positive half: the snapshot must contain exactly seed, not merely lack
	// later — an empty result would satisfy a bare "later is absent" check
	// even if the whole PIT path were broken and returned nothing at all.
	if len(got) != 1 || got[0].Meta.ID != seed {
		t.Fatalf("snapshot at instant must contain exactly [seed], got %d results", len(got))
	}
}

// TestPIT_DeleteThenRecreateInOneTx_VersionTiebreakPicksRecreated pins the
// version DESC tiebreak documented on pitBaseQueryTemplate and on GetAsAt's
// ORDER BY: Delete then Save inside one transaction stamps both the
// tombstone version and the recreated version from that transaction's
// single CURRENT_TIMESTAMP, so valid_time and transaction_time tie between
// them and version is the only column left to break the tie. Both rows are
// committed together, so a wrong pick here is not a timing accident — it is
// the tiebreak firing on the wrong column, deterministically, every run.
func TestPIT_DeleteThenRecreateInOneTx_VersionTiebreakPicksRecreated(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	baseCtx := ctxWithTenant("entity-tenant")
	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	mref := spi.ModelRef{EntityName: "pit-tiebreak", ModelVersion: "1"}

	id := uuid.NewString()
	if _, err := store.Save(baseCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref, State: "LIVE"},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	txID, txCtx := postgres.BeginGuardedForTest(t, tm, baseCtx)
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}
	if err := txStore.Delete(txCtx, id); err != nil {
		t.Fatalf("tx delete: %v", err)
	}
	if _, err := txStore.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: mref, State: "LIVE"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("tx recreate: %v", err)
	}
	if err := tm.Commit(baseCtx, txID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The recreated version's own valid_time is the instant to query at —
	// and, per the tie this test forces, it is also the tombstone's
	// valid_time and transaction_time.
	instant := readbackInstant(baseCtx, t, store, id)

	got, err := store.Search(baseCtx, spi.Filter{}, spi.SearchOptions{
		ModelName: mref.EntityName, ModelVersion: mref.ModelVersion,
		Limit: 10, PointInTime: &instant,
	})
	if err != nil {
		t.Fatalf("Search at instant: %v", err)
	}
	if len(got) != 1 || string(got[0].Data) != `{"n":1}` {
		t.Fatalf("Search at the tied instant must resolve to the recreated (higher-version) row, got %d results", len(got))
	}

	asAt, err := store.GetAsAt(baseCtx, id, instant)
	if err != nil {
		t.Fatalf("GetAsAt at instant: %v", err)
	}
	if string(asAt.Data) != `{"n":1}` {
		t.Fatalf("GetAsAt at the tied instant must resolve to the recreated (higher-version) row, got %s", asAt.Data)
	}
}
