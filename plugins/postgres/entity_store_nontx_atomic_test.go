package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestNonTxSave_IsAtomic proves a non-transactional Save leaves no partial
// state behind when one of its statements fails. A version row planted at
// version 1 makes the save's own INSERT into entity_versions violate the
// primary key (tenant_id, entity_id, version); the entities row written
// earlier in the same save must not survive that failure.
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

// TestNonTxDelete_IsAtomic proves a non-transactional Delete leaves no
// partial state behind when one of its statements fails.
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
	ctx := ctxWithTenant(tenant)
	pool := postgres.PoolForTest(factory)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	id := uuid.NewString()
	mref := spi.ModelRef{EntityName: "atomic-delete-probe", ModelVersion: "1"}

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
}
