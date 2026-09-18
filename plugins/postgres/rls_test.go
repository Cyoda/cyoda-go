package postgres_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestRLS_PoliciesExist verifies that RLS policies are defined on all tables.
// FORCE ROW LEVEL SECURITY is not set (deferred to Plan 5 when SET LOCAL is
// wired at transaction start). The table owner bypasses RLS without FORCE.
//
// The supported posture today is the table owner, with application-level
// WHERE tenant_id = $1 as the primary isolation and RLS as inert
// defence-in-depth beneath it. A non-owner, RLS-subject role is the intended
// hardening target, NOT a supported deployment: every transaction-scoped path
// sets app.current_tenant and would work under it, but no POOL-routed
// statement carries that GUC (set_config's is_local flag scopes it to a
// transaction), so under a non-owner role the pool paths would see a NULL
// setting and return nothing — silently for a read, and wrongly for
// TransactionManager.getSubmitTimeFromTable, which would report a committed
// transaction as not found. Enabling the mode means setting the tenant on the
// pool path plugin-wide first. The non-owner probe roles some tests create
// exist to prove the transaction-scoped paths are already GUC-correct.
func TestRLS_PoliciesExist(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	ctx := context.Background()

	// Driven from the schema, not a hardcoded list: a hardcoded one silently
	// stops covering every table added after it was written. It missed
	// unique_claims, model_schema_extensions and submit_times.
	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND column_name = 'tenant_id'
		 ORDER BY table_name`)
	if err != nil {
		t.Fatalf("failed to enumerate tenant-scoped tables: %v", err)
	}
	// One table is deliberately not enrolled, and it is not an oversight to
	// close: ScanDue is a trusted cross-tenant system read, so scoping
	// scheduled_tasks to a single tenant would break the scheduler the moment
	// enforcement is strengthened (FORCE + a non-owner role) — which is the
	// F7 decision, pinned from the opposite side by
	// TestPostgres_ScheduledTasksTable_NotRLSEnrolled. The two tests must
	// agree; adding an entry here without revisiting that one will simply
	// swap which of them fails.
	exempt := map[string]string{
		"scheduled_tasks": "ScanDue is a trusted cross-tenant system read (F7)",
	}

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		if reason, ok := exempt[name]; ok {
			t.Logf("skipping %s: %s", name, reason)
			continue
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("enumerate tenant-scoped tables: %v", err)
	}
	if len(tables) < 6 {
		t.Fatalf("found only %d tenant-scoped tables (%v) — the schema query is wrong, "+
			"not the schema", len(tables), tables)
	}

	for _, table := range tables {
		// Verify RLS is enabled (policies are defined)
		var rlsEnabled bool
		err := pool.QueryRow(ctx,
			"SELECT relrowsecurity FROM pg_class WHERE relname = $1", table).Scan(&rlsEnabled)
		if err != nil {
			t.Fatalf("failed to check RLS for %s: %v", table, err)
		}
		if !rlsEnabled {
			t.Errorf("RLS not enabled on table %s", table)
		}

		// Verify a tenant_isolation policy exists
		var policyCount int
		// Both naming shapes are in use: tenant_isolation_<table> in 000001 and
		// 000003, <table>_tenant_isolation in model_schema_extensions and
		// submit_times. Matching only the prefix silently passed tables whose
		// policy used the suffix form.
		err = pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_policies
			 WHERE tablename = $1
			   AND (policyname LIKE 'tenant_isolation%' OR policyname LIKE '%\_tenant\_isolation')`,
			table).Scan(&policyCount)
		if err != nil {
			t.Fatalf("failed to check policies for %s: %v", table, err)
		}
		if policyCount == 0 {
			t.Errorf("no tenant_isolation policy found on table %s", table)
		}
	}
}

// TestRLS_ApplicationLevelIsolation verifies that the KV store's application-level
// tenant filtering works correctly — tenant-A's data is invisible to tenant-B.
// This does NOT test RLS enforcement (which requires FORCE + non-owner role),
// it tests the WHERE tenant_id = $1 filtering in the store implementation.
func TestRLS_ApplicationLevelIsolation(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	factory := postgres.NewStoreFactory(pool)

	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, err := factory.KeyValueStore(ctxA)
	if err != nil {
		t.Fatalf("KeyValueStore A: %v", err)
	}
	storeB, err := factory.KeyValueStore(ctxB)
	if err != nil {
		t.Fatalf("KeyValueStore B: %v", err)
	}

	// Tenant A writes
	if err := storeA.Put(ctxA, "rls-test", "secret", []byte("tenant-A-data")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tenant B cannot see tenant A's data
	_, err = storeB.Get(ctxB, "rls-test", "secret")
	if err == nil {
		t.Fatal("tenant-B should not see tenant-A's key")
	}

	// Tenant B list is empty
	result, err := storeB.List(ctxB, "rls-test")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("tenant-B should see 0 entries, got %d", len(result))
	}

	// Tenant A can still see its own data
	got, err := storeA.Get(ctxA, "rls-test", "secret")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "tenant-A-data" {
		t.Errorf("expected 'tenant-A-data', got %q", string(got))
	}
}
