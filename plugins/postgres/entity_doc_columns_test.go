package postgres_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
	"github.com/google/uuid"
)

// TestEntityDoc_TemporalValuesAreNotInTheDocument asserts the document stops
// carrying temporal values: the columns are the source of truth, and a
// commit-phase stamp must not have to rewrite JSONB to keep them honest.
//
// Lives in package postgres_test (not entity_doc_test.go's package postgres)
// because it needs a real, migrated database via setupEntityTest/PoolForTest —
// those fixtures are defined in the external test package, which internal
// test files cannot see.
func TestEntityDoc_TemporalValuesAreNotInTheDocument(t *testing.T) {
	factory := setupEntityTest(t)
	tenant := spi.TenantID("doc-shape-tenant")
	ctx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: "doc-shape", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var meta map[string]any
	if err := pool.QueryRow(ctx,
		`SELECT doc->'_meta' FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&meta); err != nil {
		t.Fatalf("read _meta: %v", err)
	}
	// Guard against a vacuous pass: if _meta were absent or somehow decoded
	// empty, every absence check below would trivially succeed without
	// having proven anything. Assert real content first — a key that must
	// always survive this removal.
	if len(meta) == 0 {
		t.Fatal("_meta decoded empty; the absence checks below would be vacuous")
	}
	if _, ok := meta["id"]; !ok {
		t.Error("_meta missing \"id\"; the absence checks below would be vacuous if _meta is malformed rather than genuinely pruned")
	}
	if _, ok := meta["state"]; !ok {
		t.Error("_meta missing \"state\"; the absence checks below would be vacuous if _meta is malformed rather than genuinely pruned")
	}
	for _, k := range []string{"valid_time", "transaction_time", "wall_clock_time", "creation_date", "last_modified_date"} {
		if _, present := meta[k]; present {
			t.Errorf("_meta still carries %q; temporal values belong in columns", k)
		}
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.CreationDate.IsZero() || got.Meta.LastModifiedDate.IsZero() {
		t.Error("reported dates must be projected from the columns, not dropped")
	}
}

// TestGetAsAt_CreationDateSurvivesUpdate proves that every version of an
// updated entity reports the entity's ORIGINAL creation instant, not the
// instant of whichever write produced that particular version.
//
// Before this fix, neither entity_versions INSERT (saveOn's nor deleteOn's)
// wrote creation_date at all, so every version row kept the migration's
// DEFAULT CURRENT_TIMESTAMP — that version's OWN transaction-start time.
// GetAsAt/GetVersionByTransaction/the PIT lateral all project the column
// (this task), so a second-or-later version reported the UPDATE's instant as
// its creation date, not the entity's real one. The fix sources
// entity_versions.creation_date from entities.creation_date via a sub-select
// at INSERT time — the entities row's creation_date is never touched by any
// UPDATE, so it stays the true original for the entity's whole lifetime.
func TestGetAsAt_CreationDateSurvivesUpdate(t *testing.T) {
	factory := setupEntityTest(t)
	tenant := spi.TenantID("creation-date-survives-tenant")
	ctx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	ref := spi.ModelRef{EntityName: "creation-date-survives", ModelVersion: "1"}

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("create Save: %v", err)
	}

	created, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	createInstant := created.Meta.CreationDate
	if createInstant.IsZero() {
		t.Fatal("created.Meta.CreationDate is zero")
	}

	// Space create and update into distinct instants — boundaries come from
	// the DB clock, never time.Now() (see pit_time_test.go); the sleep here
	// only needs to separate the two writes, not name an instant.
	time.Sleep(2 * time.Millisecond)

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref},
		Data: []byte(`{"n":2}`),
	}); err != nil {
		t.Fatalf("update Save: %v", err)
	}
	afterUpdate := dbNow(t, ctx, pool)

	got, err := store.GetAsAt(ctx, id, afterUpdate)
	if err != nil {
		t.Fatalf("GetAsAt: %v", err)
	}
	if got.Meta.Version != 2 {
		t.Fatalf("GetAsAt returned version %d, want 2 (the update) — test setup problem, not the fix under test", got.Meta.Version)
	}
	if !got.Meta.CreationDate.Equal(createInstant) {
		t.Errorf("GetAsAt(v2).CreationDate = %v, want the create instant %v (not the update's)", got.Meta.CreationDate, createInstant)
	}
}

// TestLastModified_AdvancesOnUpdateAndDelete proves entities.last_modified is
// not frozen at the entity's creation forever: an update and a delete must
// each stamp it forward.
//
// Before this fix, none of the three statements that touch the entities row
// (the upsert's ON CONFLICT DO UPDATE, the follow-up doc-only UPDATE, and
// deleteOn's UPDATE) ever set last_modified, so it kept whatever the
// migration's DEFAULT CURRENT_TIMESTAMP gave it at the row's original
// INSERT — permanently, no matter how many updates or the eventual delete
// followed. Get/GetPage/Search project this column (this task), so that
// defect was invisible only because nothing read it back until now.
//
// Asserts the PROPERTY (the value advanced), never a specific instant:
// these columns still hold only a provisional stamp — CURRENT_TIMESTAMP /
// dbNow at write time — until the commit-phase stamping task lands.
func TestLastModified_AdvancesOnUpdateAndDelete(t *testing.T) {
	factory := setupEntityTest(t)
	tenant := spi.TenantID("last-modified-advances-tenant")
	ctx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	id := uuid.NewString()
	ref := spi.ModelRef{EntityName: "last-modified-advances", ModelVersion: "1"}

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("create Save: %v", err)
	}
	created, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	createdLM := created.Meta.LastModifiedDate
	if createdLM.IsZero() {
		t.Fatal("created.Meta.LastModifiedDate is zero")
	}

	// Space create/update/delete into distinct instants — boundaries come
	// from the DB clock, never time.Now() (see pit_time_test.go); the sleep
	// here only needs to separate the writes, not name an instant.
	time.Sleep(2 * time.Millisecond)

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: ref},
		Data: []byte(`{"n":2}`),
	}); err != nil {
		t.Fatalf("update Save: %v", err)
	}
	updated, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	updatedLM := updated.Meta.LastModifiedDate
	if !updatedLM.After(createdLM) {
		t.Errorf("update did not advance LastModifiedDate: created=%v updated=%v", createdLM, updatedLM)
	}

	time.Sleep(2 * time.Millisecond)

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Get would report ENTITY_NOT_FOUND for a soft-deleted row — read the
	// column directly, the same way migration_backfill_test.go does.
	var deletedLM time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_modified FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&deletedLM); err != nil {
		t.Fatalf("read last_modified after delete: %v", err)
	}
	if !deletedLM.After(updatedLM) {
		t.Errorf("delete did not advance LastModifiedDate: updated=%v deleted=%v", updatedLM, deletedLM)
	}
}
