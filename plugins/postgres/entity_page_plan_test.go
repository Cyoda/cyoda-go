package postgres_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// explainPlan runs "EXPLAIN (FORMAT TEXT) <query>" over conn and returns the
// full plan text (all rows joined with newlines) so callers can assert on
// the plan shape with strings.Contains checks.
func explainPlan(t *testing.T, ctx context.Context, conn *pgxpool.Conn, query string, args ...any) string {
	t.Helper()
	rows, err := conn.Query(ctx, "EXPLAIN (FORMAT TEXT) "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan row iteration: %v", err)
	}
	return strings.Join(lines, "\n")
}

// seedPagePlanEntities saves n entities under mref via the real EntityStore
// (so doc/version/timestamps are the plugin's own shapes, not hand-crafted
// rows), returning nothing — the plan tests only need populated tables, not
// specific IDs.
func seedPagePlanEntities(t *testing.T, factory *postgres.StoreFactory, tenant spi.TenantID, mref spi.ModelRef, n int) {
	t.Helper()
	ctx := ctxWithTenant(tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for i := 0; i < n; i++ {
		e := &spi.Entity{
			Meta: spi.EntityMeta{
				ID:       idForPlanTest(i),
				ModelRef: mref,
			},
			Data: []byte(`{"i":` + strconv.Itoa(i) + `}`),
		}
		if _, err := store.Save(ctx, e); err != nil {
			t.Fatalf("Save(%d): %v", i, err)
		}
	}
}

func idForPlanTest(i int) string { return "plan-entity-" + strconv.Itoa(i) }

// TestGetPage_NonTx_UsesModelEntityIDIndex is query-shape evidence for
// GetPage's asAt==nil path (entity_store.go's getPageCurrent): idx_entities_
// model_entity_id (migrations/000008_entities_model_entity_id_index.up.sql)
// must serve BOTH the tenant/model/model-version equality filter AND the
// ORDER BY entity_id COLLATE "C", without a separate sort step.
func TestGetPage_NonTx_UsesModelEntityIDIndex(t *testing.T) {
	factory := setupEntityTest(t)
	tenant := spi.TenantID("plan-tenant")
	mref := spi.ModelRef{EntityName: "PlanOrder", ModelVersion: "1"}
	seedPagePlanEntities(t, factory, tenant, mref, 20)

	pool := postgres.PoolForTest(factory)
	if pool == nil {
		t.Skip("PoolForTest not available")
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "ANALYZE entities"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// Force the planner to prefer the index over a seq scan — on a 20-row
	// table the cost crossover to a seq scan has not been reached, so
	// without this the plan would say nothing about whether the index is
	// actually usable for this query shape.
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}

	plan := explainPlan(t, ctx, conn,
		`SELECT doc FROM entities
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
		 ORDER BY entity_id COLLATE "C"
		 LIMIT $4 OFFSET $5`,
		string(tenant), mref.EntityName, mref.ModelVersion, 10, 0)

	if !strings.Contains(plan, "idx_entities_model_entity_id") {
		t.Errorf("GetPage non-tx query must use idx_entities_model_entity_id, got plan:\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Errorf("GetPage non-tx query must not need a separate sort step (the index already covers ORDER BY entity_id), got plan:\n%s", plan)
	}
}

// TestGetVersionByTransaction_StaysWithinEntityVersionsPK is query-shape
// evidence that GetVersionByTransaction's lookup scopes to the target
// entity's own version range via entity_versions' PRIMARY KEY (tenant_id,
// entity_id, version), rather than scanning the whole table.
func TestGetVersionByTransaction_StaysWithinEntityVersionsPK(t *testing.T) {
	factory := setupEntityTest(t)
	tenant := spi.TenantID("plan-tenant-gvbt")
	mref := spi.ModelRef{EntityName: "PlanOrder", ModelVersion: "1"}
	seedPagePlanEntities(t, factory, tenant, mref, 20)

	pool := postgres.PoolForTest(factory)
	if pool == nil {
		t.Skip("PoolForTest not available")
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "ANALYZE entity_versions"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}

	plan := explainPlan(t, ctx, conn,
		`SELECT doc, version, valid_time FROM entity_versions
		 WHERE tenant_id = $1 AND entity_id = $2
		   AND doc->'_meta'->>'transaction_id' = $3
		   AND (doc->'_meta'->>'deleted')::boolean IS NOT TRUE
		 ORDER BY version ASC
		 LIMIT 1`,
		string(tenant), idForPlanTest(0), "tx-does-not-matter")

	if !strings.Contains(plan, "entity_versions_pkey") {
		t.Errorf("GetVersionByTransaction query must use entity_versions' PRIMARY KEY index, got plan:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on entity_versions") {
		t.Errorf("GetVersionByTransaction query must not full-scan entity_versions, got plan:\n%s", plan)
	}
}
