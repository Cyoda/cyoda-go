package sqlite_test

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestSMAuditEvents_HasTenantTransactionIndex asserts sm_audit_events carries an
// index whose leading columns are (tenant_id, transaction_id) — the WHERE the
// commit phase stamps audit events by (flushToSQLite).
//
// The index 000001 created, idx_sm_events_tx, is
// (tenant_id, entity_id, transaction_id). entity_id sits between the two
// columns the stamp constrains, and SQLite, like any B-tree, can only use a
// leading prefix — so that index degrades the stamp to a scan of every audit
// row the tenant owns, on every commit. The postgres plugin carries the
// identical assertion against the identical defect; both backends inherited it
// from the same 000001 schema.
//
// A schema property rather than an EXPLAIN QUERY PLAN assertion: on a small
// table SQLite may legitimately choose a scan whatever indexes exist, so a plan
// assertion would either be flaky or need a disproportionate amount of seeded
// data. The index's own definition is deterministic.
//
// The assertion is on the PREFIX, so a future index that leads with these two
// columns and adds more still satisfies it.
func TestSMAuditEvents_HasTenantTransactionIndex(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "audit-index.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	db := sqlite.DBForTest(factory)
	// sql IS NULL for indexes SQLite creates implicitly; those carry no
	// column list to match against and are not what this guards.
	rows, err := db.Query(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'sm_audit_events' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("list sm_audit_events indexes: %v", err)
	}
	defer rows.Close()

	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan index sql: %v", err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index definitions: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("sm_audit_events has no explicit indexes at all; the assertion below would be vacuous")
	}

	prefix := regexp.MustCompile(`\(\s*tenant_id\s*,\s*transaction_id\s*[,)]`)
	for _, def := range defs {
		if prefix.MatchString(def) {
			return
		}
	}
	t.Errorf("sm_audit_events has no index whose leading columns are (tenant_id, transaction_id); "+
		"the commit-phase stamp filters on exactly those two, so without one every commit scans "+
		"every audit row of the tenant. Indexes present:\n%v", defs)
}
