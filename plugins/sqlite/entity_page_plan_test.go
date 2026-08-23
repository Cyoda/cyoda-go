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
func TestGetPage_NonTx_UsesModelIDIndex(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "plan.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	db := sqlite.DBForTest(factory)
	plan := explainQueryPlan(t, db,
		`SELECT entity_id, model_name, model_version, version,
		        json(data), json(meta), created_at, updated_at
		 FROM entities
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted
		 ORDER BY entity_id
		 LIMIT ? OFFSET ?`,
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
func TestGetVersionByTransaction_StaysWithinEntityVersions(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "plan.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	db := sqlite.DBForTest(factory)
	plan := explainQueryPlan(t, db,
		`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
		        json(ev.data), json(ev.meta), ev.submit_time,
		        ev.change_type, ev.user_id, ev.transaction_id
		 FROM entity_versions ev
		 WHERE ev.tenant_id = ? AND ev.entity_id = ? AND ev.transaction_id = ?
		   AND ev.change_type != 'DELETED'
		 ORDER BY ev.version ASC
		 LIMIT 1`,
		"t1", "e1", "tx1")

	if !strings.Contains(plan, "USING PRIMARY KEY") || !strings.Contains(plan, "entity_id") {
		t.Errorf("GetVersionByTransaction query must search entity_versions' PRIMARY KEY scoped by entity_id, got plan: %s", plan)
	}
	if strings.Contains(plan, "SCAN ev") {
		t.Errorf("GetVersionByTransaction query must not full-scan entity_versions, got plan: %s", plan)
	}
}
