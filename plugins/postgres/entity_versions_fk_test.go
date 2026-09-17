package postgres_test

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestEntityVersionsRequireAnEntityRow proves the foreign key holds. A version
// row without its entity row is invisible to every point-in-time read — the
// read enumerates entities and probes each one's revision — so it must be
// impossible to create, not merely absent by convention. A fixture in this
// repository violated the convention before the constraint existed, and
// nothing detected it until a query change made the rows disappear.
func TestEntityVersionsRequireAnEntityRow(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("t-fk")
	pool := postgres.PoolForTest(factory)

	_, err := pool.Exec(ctx,
		`INSERT INTO entity_versions
		   (tenant_id, entity_id, model_name, model_version, version, valid_time, creation_date, doc)
		 VALUES ('t-fk', 'orphan-id', 'm', '1', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '{"_meta":{}}'::jsonb)`)
	if err == nil {
		t.Fatal("an entity_versions row with no entities row must be rejected by the foreign key")
	}
	if !strings.Contains(err.Error(), "entity_versions_entity_fk") {
		t.Errorf("expected the foreign key to reject it, got: %v", err)
	}
	t.Logf("orphan insert rejected: %v", err)
}

// TestEntityVersionsAcceptAPairedEntityRow proves the foreign key is
// discriminating, not merely broken in a way that rejects everything. A
// constraint whose validation always errors (a typo'd column, an always-false
// USING clause) would pass TestEntityVersionsRequireAnEntityRow too, since
// that test only ever checks for rejection.
func TestEntityVersionsAcceptAPairedEntityRow(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("t-fk-paired")
	pool := postgres.PoolForTest(factory)

	if _, err := pool.Exec(ctx,
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		 VALUES ('t-fk-paired', 'paired-id', 'm', '1', 1, false, '{"_meta":{}}'::jsonb)`); err != nil {
		t.Fatalf("seed entities row: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO entity_versions
		   (tenant_id, entity_id, model_name, model_version, version, valid_time, creation_date, doc)
		 VALUES ('t-fk-paired', 'paired-id', 'm', '1', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '{"_meta":{}}'::jsonb)`); err != nil {
		t.Errorf("a version row with a matching entities row must be accepted, got: %v", err)
	}
}

// TestEntityVersionsEntityFK_RestrictsEntityDeletion proves the foreign key is
// ON DELETE RESTRICT, not CASCADE. The spec's own reason for this constraint —
// so a future retention or erasure feature cannot silently take an entity's
// history with it — is exactly what CASCADE would defeat: it would let one
// DELETE FROM entities remove the whole version chain with no error and no
// chance to object. RESTRICT must refuse the deletion outright while a
// version row still references it.
func TestEntityVersionsEntityFK_RestrictsEntityDeletion(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("t-fk-restrict")
	pool := postgres.PoolForTest(factory)

	if _, err := pool.Exec(ctx,
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		 VALUES ('t-fk-restrict', 'restrict-id', 'm', '1', 1, false, '{"_meta":{}}'::jsonb)`); err != nil {
		t.Fatalf("seed entities row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO entity_versions
		   (tenant_id, entity_id, model_name, model_version, version, valid_time, creation_date, doc)
		 VALUES ('t-fk-restrict', 'restrict-id', 'm', '1', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '{"_meta":{}}'::jsonb)`); err != nil {
		t.Fatalf("seed entity_versions row: %v", err)
	}

	_, err := pool.Exec(ctx,
		`DELETE FROM entities WHERE tenant_id = 't-fk-restrict' AND entity_id = 'restrict-id'`)
	if err == nil {
		t.Fatal("deleting an entities row with a referencing entity_versions row must be rejected " +
			"(ON DELETE RESTRICT) — a silent CASCADE would take the version's history with it")
	}
	if !strings.Contains(err.Error(), "entity_versions_entity_fk") {
		t.Errorf("expected the foreign key to reject the deletion, got: %v", err)
	}
	t.Logf("entities deletion restricted: %v", err)

	var versionRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_versions WHERE tenant_id = 't-fk-restrict' AND entity_id = 'restrict-id'`,
	).Scan(&versionRows); err != nil {
		t.Fatalf("count entity_versions rows: %v", err)
	}
	if versionRows != 1 {
		t.Errorf("entity_versions row count = %d, want 1 (must survive the rejected deletion)", versionRows)
	}
}
