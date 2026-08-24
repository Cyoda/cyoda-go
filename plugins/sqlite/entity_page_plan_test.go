package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// explainQueryPlan runs "EXPLAIN QUERY PLAN <query>" and returns every
// row's "detail" column (SQLite's plan-step description), joined so callers
// can assert on the whole plan shape with strings.Contains checks.
func explainQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan row iteration: %v", err)
	}
	return strings.Join(details, " | ")
}

// TestGetPage_NonTx_UsesModelIDIndex is query-shape evidence for Q3's
// non-tx GetPage path: idx_entities_model_id (migrations/000006_search_epoch.up.sql)
// must serve BOTH the tenant/model/model-version equality filter AND the
// entity_id ORDER BY, without a separate sort step (no "USE TEMP B-TREE
// FOR ORDER BY" in the plan).
//
// The query comes from the production constant, not a copy of it: this test
// backs a coverage waiver (see internal/e2e/async_stream_test.go), so it must
// fail when the production query drifts rather than keep asserting the plan
// of a query nothing executes any more.
func TestGetPage_NonTx_UsesModelIDIndex(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "plan.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	db := sqlite.DBForTest(factory)
	plan := explainQueryPlan(t, db, sqlite.GetPageDirectQueryForTest,
		"t1", "m1", "v1", 10, 0)

	if !strings.Contains(plan, "idx_entities_model_id") {
		t.Errorf("GetPage non-tx query must use idx_entities_model_id, got plan: %s", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Errorf("GetPage non-tx query must not need a separate sort step (index already covers ORDER BY entity_id), got plan: %s", plan)
	}
}

// TestGetVersionByTransaction_StaysWithinEntityVersions is query-shape
// evidence that GetVersionByTransaction's lookup scopes to the target
// entity's own version range (via entity_versions' PRIMARY KEY
// (tenant_id, entity_id, version)) rather than scanning the whole table.
//
// As above, the query comes from the production constant so the assertion
// cannot outlive the query it describes.
func TestGetVersionByTransaction_StaysWithinEntityVersions(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "plan.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	db := sqlite.DBForTest(factory)
	plan := explainQueryPlan(t, db, sqlite.GetVersionByTransactionQueryForTest,
		"t1", "e1", "tx1")

	if !strings.Contains(plan, "USING PRIMARY KEY") || !strings.Contains(plan, "entity_id") {
		t.Errorf("GetVersionByTransaction query must search entity_versions' PRIMARY KEY scoped by entity_id, got plan: %s", plan)
	}
	if strings.Contains(plan, "SCAN ev") {
		t.Errorf("GetVersionByTransaction query must not full-scan entity_versions, got plan: %s", plan)
	}
}
