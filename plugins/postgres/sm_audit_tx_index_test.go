package postgres_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestSMAuditEvents_HasTenantTransactionIndex asserts sm_audit_events carries an
// index whose leading columns are (tenant_id, transaction_id) — the WHERE the
// commit phase stamps audit events by (TransactionManager.stampCommitInstant).
//
// The original idx_sm_events_tx is (tenant_id, entity_id, transaction_id).
// entity_id sits between the two columns the stamp actually constrains, and a
// B-tree can only use a leading prefix, so that index degrades the stamp to a
// scan of every audit row the tenant owns — on EVERY commit, including a
// read-only one. The stamp is new; the whole-tenant scan it implied was new
// with it.
//
// This is a schema property, deliberately not a planner assertion: on a small,
// freshly-ANALYZEd table a sequential scan is the planner's correct choice no
// matter which indexes exist, and seeding enough audit rows to force an index
// scan on every CI run would be disproportionate — the same reasoning
// TestEntitiesModelEntityIDIndex_HasNoPartialPredicate records for the index it
// guards. Asserting the index's own definition is deterministic and cheap.
//
// The assertion is on the PREFIX, not on an exact index name or column list: a
// future index that starts with these two columns and adds more still serves
// the stamp, and pinning the exact definition would fail a harmless widening.
func TestSMAuditEvents_HasTenantTransactionIndex(t *testing.T) {
	factory := setupEntityTest(t)
	pool := postgres.PoolForTest(factory)
	ctx := context.Background()

	rows, err := pool.Query(ctx,
		"SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'sm_audit_events'")
	if err != nil {
		t.Fatalf("list sm_audit_events indexes: %v", err)
	}
	defer rows.Close()

	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan indexdef: %v", err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexdefs: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("sm_audit_events has no indexes at all; the assertion below would be vacuous")
	}

	// (tenant_id, transaction_id, ...) — the two stamped columns as the
	// leading prefix, in that order, with anything or nothing after them.
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
