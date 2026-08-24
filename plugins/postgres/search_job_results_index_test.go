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
// for BOTH halves of migration 000009: that the widened PK actually serves
// GetResultIDs' tenant+job lookup, and that idx_search_job_results_tenant —
// which that lookup used to need — is not chosen for any part of it.
//
// It EXPLAINs postgres.GetResultIDsQueryForTest, the PRODUCTION constant, not a
// simplified re-typing: the real query is a `WITH total AS (…)` CTE feeding a
// LEFT JOIN LATERAL, and a flat `SELECT … ORDER BY seq LIMIT` says nothing
// about how those two arms plan.
//
// The assertions are the ones that distinguish the pre- and post-migration
// schemas. Merely finding the string "search_job_results_pkey" somewhere in the
// plan does NOT: under the old (job_id, seq) PK the planner still names the
// pkey, with tenant_id demoted to a `Filter:` — so the three shapes (old PK +
// index, new PK + index, new PK alone) all contain that string. What separates
// them is WHICH index each scan uses, whether tenant_id reaches the Index Cond,
// and whether the ORDER BY still needs a Sort.
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

	// $1 job, $2 tenant, $3 offset, $4 limit — GetResultIDs' own argument order.
	plan := explainPlan(t, ctx, conn, postgres.GetResultIDsQueryForTest,
		"job-x", "tenant-x", 0, 10)

	if strings.Contains(plan, "idx_search_job_results_tenant") {
		t.Errorf("a search_job_results scan still plans on idx_search_job_results_tenant, so migration 000009's "+
			"claim that the PK subsumes it is false; plan:\n%s", plan)
	}

	lines := strings.Split(plan, "\n")
	scans := 0
	for i, line := range lines {
		// Matches "… Scan using <index> on search_job_results [alias]" and
		// never search_jobs, whose name is not a prefix of this one.
		if !strings.Contains(line, " on search_job_results") {
			continue
		}
		scans++
		if !strings.Contains(line, "using search_job_results_pkey") {
			t.Errorf("a search_job_results scan does not use the PK: %q\nplan:\n%s", strings.TrimSpace(line), plan)
			continue
		}
		if i+1 >= len(lines) || !strings.Contains(lines[i+1], "Index Cond:") {
			t.Errorf("scan %q has no Index Cond — the PK is being scanned, not probed\nplan:\n%s",
				strings.TrimSpace(line), plan)
			continue
		}
		cond := lines[i+1]
		// Both columns in the Index Cond is what the widened PK bought. Under
		// the old (job_id, seq) PK, tenant_id appears as a `Filter:` on a
		// separate line instead — the case the old string-contains assertion
		// could not see.
		if !strings.Contains(cond, "tenant_id") || !strings.Contains(cond, "job_id") {
			t.Errorf("Index Cond %q does not cover both tenant_id and job_id — the PK is not being used "+
				"as a (tenant_id, job_id) probe\nplan:\n%s", strings.TrimSpace(cond), plan)
		}
	}
	// The count CTE and the LATERAL page are both expected to scan the table.
	if scans != 2 {
		t.Errorf("expected 2 search_job_results scans (the count CTE and the LATERAL page), got %d\nplan:\n%s", scans, plan)
	}
	// seq trails (tenant_id, job_id) in the PK, so ORDER BY seq is satisfied by
	// the index order itself: an OFFSET/LIMIT page reads only the rows it
	// returns. A Sort node means the whole job's result set is being
	// materialised and ordered per page request instead.
	if strings.Contains(plan, "->  Sort") {
		t.Errorf("the page read still needs an explicit Sort for ORDER BY seq — the (tenant_id, job_id, seq) PK "+
			"should supply that order directly\nplan:\n%s", plan)
	}
}
