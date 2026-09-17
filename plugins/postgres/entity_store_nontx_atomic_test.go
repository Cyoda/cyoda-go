package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestNonTxSave_IsAtomic proves a non-transactional Save leaves no partial
// state behind when one of its statements fails.
//
// The collision used to be a bare orphan entity_versions row (a version 1
// with no entities row at all), which forced Save's own entities upsert
// down its fresh-insert path so its own version-1 write would collide. That
// is no longer constructible: entity_versions_entity_fk (migration 000012)
// requires every entity_versions row to have an entities row, so an orphan
// can no longer exist to plant. Planting the entities row too, at version 0,
// reaches the same collision through the update path instead — Save's
// upsert computes nextVersion = 0 + 1 = 1, which still collides with the
// version-1 row already planted — and is arguably the more representative
// case anyway, since real writers race on updates far more often than on an
// entity's very first save.
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

	// Plant the entities row the foreign key now requires, at version 0 so
	// Save's upsert lands on nextVersion = 1 below.
	if _, err := pool.Exec(ctx,
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		 VALUES ($1, $2, $3, $4, 0, false, '{"_meta":{}}'::jsonb)`,
		string(tenant), id, mref.EntityName, mref.ModelVersion); err != nil {
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

	var version int64
	if err := pool.QueryRow(ctx,
		`SELECT version FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		string(tenant), id).Scan(&version); err != nil {
		t.Fatalf("query entities row: %v", err)
	}
	if version != 0 {
		t.Errorf("entities row was updated despite a failed non-tx Save: got version %d, want 0 (unchanged) — "+
			"the save's statements are not atomic", version)
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
