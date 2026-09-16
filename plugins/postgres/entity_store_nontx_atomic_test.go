package postgres_test

import (
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
