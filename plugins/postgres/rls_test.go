package postgres_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestRLS_PoliciesExist verifies that RLS policies are defined on all tables.
// FORCE ROW LEVEL SECURITY is not set (deferred to Plan 5 when SET LOCAL is
// wired at transaction start). The table owner bypasses RLS without FORCE.
// In production, the application should connect as a non-owner role for RLS
// enforcement. Application-level WHERE tenant_id = $1 is the primary isolation.
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

	tables := []string{"entities", "entity_versions", "sm_audit_events", "models", "kv_store", "messages"}
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
		err = pool.QueryRow(ctx,
			"SELECT count(*) FROM pg_policies WHERE tablename = $1 AND policyname LIKE 'tenant_isolation%'", table).Scan(&policyCount)
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
