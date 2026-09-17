package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestNonTxSave_IsAtomic proves a non-transactional Save leaves no partial
// state behind when its version insert fails on a brand-new entity — the
// fresh-insert branch of saveOn's upsert.
//
// The collision used to be a bare orphan entity_versions row (a version 1
// with no entities row at all), which forced Save's own entities upsert down
// its fresh-insert path so its own version-1 write would collide with the
// planted one. That plant is no longer constructible: entity_versions_entity_fk
// (migration 000012) requires every entity_versions row to have an entities
// row, so an orphan can no longer exist to seed a collision with. A temporary
// CHECK constraint reaches the same failure without one: it blocks the
// version-1 insert Save's own fresh-create path performs, deterministically,
// with no orphan row and no timing dependency — the same recipe
// TestNonTxDelete_IsAtomic already uses for the write a version-collision
// can't reach.
func TestNonTxSave_IsAtomic(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-nontx-atomic"
	ctx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "atomic-probe", ModelVersion: "1"}

	// No entities row exists for this id yet, so the constraint only ever
	// has to reject version 1 — the one saveOn's fresh-insert path is about
	// to try.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE entity_versions ADD CONSTRAINT block_version_insert CHECK (version <> 1)`); err != nil {
		t.Fatalf("add blocking constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE entity_versions DROP CONSTRAINT IF EXISTS block_version_insert`)
	})

	_, saveErr := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: mref},
		Data: []byte(`{"n":1}`),
	})
	if saveErr == nil {
		t.Fatal("Save must fail: the entity_versions insert violates the blocking CHECK constraint")
	}

	var entitiesRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&entitiesRows); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if entitiesRows != 0 {
		t.Errorf("entities row survived a failed non-tx Save: got %d rows, want 0 — "+
			"the save's statements are not atomic", entitiesRows)
	}
}

// TestNonTxSave_UpdatePathIsAtomic covers the OTHER branch of saveOn's
// upsert: a Save against an entity that already exists. Both branches share
// the same non-tx wrapping and the same failure point (the version insert),
// but only the fresh-insert branch (above) had a test before this task —
// this one closes that gap rather than silently leaving it uncovered.
//
// The collision here is a version row already at version 1 while the
// matching entities row sits at version 0 (planted directly, satisfying
// entity_versions_entity_fk): Save's upsert computes nextVersion = 0 + 1 = 1,
// which collides with the version-1 row already planted.
func TestNonTxSave_UpdatePathIsAtomic(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-nontx-atomic-update"
	ctx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "atomic-probe-update", ModelVersion: "1"}
	const plantedDoc = `{"_meta":{},"marker":"pre-save"}`

	// Plant the entities row the foreign key now requires, at version 0 so
	// Save's upsert lands on nextVersion = 1 below.
	if _, err := pool.Exec(ctx,
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		 VALUES ($1, $2, $3, $4, 0, false, $5::jsonb)`,
		string(tenant), id, mref.EntityName, mref.ModelVersion, plantedDoc); err != nil {
		t.Fatalf("plant entities row: %v", err)
	}

	// Plant the collision: version 1 already exists for this id.
	if _, err := pool.Exec(ctx,
		`INSERT INTO entity_versions
		   (tenant_id, entity_id, model_name, model_version, version, valid_time, doc)
		 VALUES ($1, $2, $3, $4, 1, CURRENT_TIMESTAMP, '{"_meta":{}}'::jsonb)`,
		string(tenant), id, mref.EntityName, mref.ModelVersion); err != nil {
		t.Fatalf("plant version row: %v", err)
	}

	_, saveErr := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: mref},
		Data: []byte(`{"n":1}`),
	})
	if saveErr == nil {
		t.Fatal("Save must fail: version 1 already exists for this entity")
	}

	// Widened beyond "version is unchanged": saveOn's upsert also overwrites
	// doc with the fully marshaled document (containing the NEW write) before
	// the version insert that then fails. If the transaction wrapping didn't
	// roll that back too, a reader would see the entity holding data from a
	// save that never actually completed — the doc reverting to exactly what
	// was planted is what proves that half of atomicity, not just the
	// version counter.
	var version int64
	var doc []byte
	if err := pool.QueryRow(ctx,
		`SELECT version, doc FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&version, &doc); err != nil {
		t.Fatalf("query entities row: %v", err)
	}
	if version != 0 {
		t.Errorf("entities.version = %d, want 0 (unchanged) — the save's statements are not atomic", version)
	}
	if !jsonEqual(doc, []byte(plantedDoc)) {
		t.Errorf("entities.doc = %s, want the pre-save planted doc %s — "+
			"the save's statements are not atomic (a partially-applied write is observable)", doc, plantedDoc)
	}
}

// TestNonTxDelete_IsAtomic proves a non-transactional Delete leaves no
// partial state behind when one of its statements fails — not just that the
// tombstone version row is rolled back, but that the entities row and its
// unique-key claim are exactly as they were before Delete ran.
//
// Delete's own write order is version-insert-then-entities-update — the
// mirror image of Save's entities-then-version order — so the version-row
// collision trick that works for Save (entity_store_nontx_atomic_test.go's
// TestNonTxSave_IsAtomic) would only fail Delete's FIRST write, before the
// entities row is ever touched, proving nothing: confirmed by running
// exactly that variant against this code before writing this test — it
// passed with zero implementation changes.
//
// This test instead adds a CHECK constraint that Delete's own UPDATE (which
// sets deleted = true) violates, deterministically failing Delete's SECOND
// write after its FIRST write — the version INSERT — has already committed:
// outside a transaction every statement this store issues auto-commits on
// its own, so that INSERT is durable the instant it returns, regardless of
// what happens next.
func TestNonTxDelete_IsAtomic(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant spi.TenantID = "tenant-nontx-delete-atomic"
	baseCtx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "atomic-delete-probe", ModelVersion: "1"}
	// A declared unique key gives releaseClaims a real claim row to delete
	// (and, when Delete fails, a real claim row that must survive) — without
	// one, releaseClaims is a no-op and that half of atomicity goes unchecked.
	keys := []spi.UniqueKey{{ID: "n-key", Fields: []string{"$.n"}}}
	ctx := spi.WithUniqueKeys(baseCtx, keys)

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: tenant, ModelRef: mref},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// The seed row already satisfies NOT deleted, so ADD CONSTRAINT validates
	// clean; Delete's UPDATE, which flips deleted to true, is what trips it.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE entities ADD CONSTRAINT block_delete_update CHECK (NOT deleted)`); err != nil {
		t.Fatalf("add blocking constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE entities DROP CONSTRAINT IF EXISTS block_delete_update`)
	})

	if deleteErr := store.Delete(ctx, id); deleteErr == nil {
		t.Fatal("Delete must fail: the entities UPDATE violates the blocking CHECK constraint")
	}

	var versionRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2 AND version = 2`,
		string(tenant), id).Scan(&versionRows); err != nil {
		t.Fatalf("count tombstone version row: %v", err)
	}
	if versionRows != 0 {
		t.Errorf("tombstone version row survived a failed non-tx Delete: got %d rows, want 0 — "+
			"the delete's statements are not atomic", versionRows)
	}

	// The entities row itself must be exactly as the seed Save left it: still
	// present, not deleted, still at version 1 — Delete's UPDATE (which would
	// have set version=2, deleted=true) never should have taken effect.
	var entitiesVersion int64
	var entitiesDeleted bool
	if err := pool.QueryRow(ctx,
		`SELECT version, deleted FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&entitiesVersion, &entitiesDeleted); err != nil {
		t.Fatalf("read back entities row: %v", err)
	}
	if entitiesDeleted {
		t.Errorf("entities row was marked deleted despite a failed non-tx Delete — " +
			"the delete's statements are not atomic")
	}
	if entitiesVersion != 1 {
		t.Errorf("entities.version = %d, want 1 (Delete's UPDATE must not have applied)", entitiesVersion)
	}

	// The unique-claims row releaseClaims would have deleted must survive
	// too: a failed Delete rolling back its entities/version writes but not
	// its claims write would free the value for a NEW entity to claim while
	// the original (undeleted) entity still holds it — a different shape of
	// the same non-atomicity this test guards.
	var claimRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM unique_claims WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&claimRows); err != nil {
		t.Fatalf("count unique_claims rows: %v", err)
	}
	if claimRows != 1 {
		t.Errorf("unique_claims rows for the entity = %d, want 1 — "+
			"releaseClaims's delete was not rolled back with the rest of the failed Delete", claimRows)
	}
}
