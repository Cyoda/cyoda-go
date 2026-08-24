package postgres_test

// search_job_results_index_test.go — the index set on search_job_results after
// migration 000009 widened its PRIMARY KEY.

import (
	"context"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestMigration_SearchJobResultsHasNoRedundantTenantIndex asserts that
// idx_search_job_results_tenant (tenant_id, job_id) — created by migration
// 000001, when the PRIMARY KEY was (job_id, seq) and a tenant-scoped lookup had
// no other index to use — is gone.
//
// Migration 000009 widened the PK to (tenant_id, job_id, seq), of which the old
// index is now a strict PREFIX: every lookup it could serve, the PK serves at
// least as well. Keeping it costs a second B-tree write on every result row
// inserted by SaveResults' CopyFrom and buys nothing.
func TestMigration_SearchJobResultsHasNoRedundantTenantIndex(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE tablename = 'search_job_results' AND indexname = $1)`,
		"idx_search_job_results_tenant").Scan(&exists); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if exists {
		t.Error("idx_search_job_results_tenant still exists — it is a strict prefix of the " +
			"(tenant_id, job_id, seq) PRIMARY KEY migration 000009 installs, so it serves no lookup the " +
			"PK does not and only slows every result insert")
	}

	// The PK must actually be the widened one — otherwise this test would pass
	// vacuously on a schema that dropped the index without gaining the PK.
	var pkDef string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'search_job_results' AND indexname = 'search_job_results_pkey'`,
	).Scan(&pkDef); err != nil {
		t.Fatalf("read PK definition: %v", err)
	}
	for _, col := range []string{"tenant_id", "job_id", "seq"} {
		if !strings.Contains(pkDef, col) {
			t.Fatalf("search_job_results_pkey = %q, want a (tenant_id, job_id, seq) key", pkDef)
		}
	}
}

// TestMigration_SearchJobResultsPagedReadUsesPK is the query-shape evidence
// that dropping idx_search_job_results_tenant costs nothing: GetResultIDs'
// tenant+job lookup, which that index existed to serve, plans on
// search_job_results_pkey instead.
func TestMigration_SearchJobResultsPagedReadUsesPK(t *testing.T) {
	factory := setupSearchTest(t)
	pool := postgres.PoolForTest(factory)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "ANALYZE search_job_results"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// The table is empty here, so without this the planner would pick a seq
	// scan on cost and the plan would say nothing about index usability.
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}

	plan := explainPlan(t, ctx, conn,
		`SELECT entity_id FROM search_job_results
		 WHERE job_id = $1 AND tenant_id = $2
		 ORDER BY seq OFFSET $3 LIMIT $4`,
		"job-x", "tenant-x", 0, 10)

	if !strings.Contains(plan, "search_job_results_pkey") {
		t.Errorf("GetResultIDs' page lookup must plan on search_job_results_pkey, got plan:\n%s", plan)
	}
}
