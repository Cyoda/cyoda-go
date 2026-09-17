package postgres_test

import (
	"testing"

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
