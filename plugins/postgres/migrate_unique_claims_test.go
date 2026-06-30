package postgres_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestMigrate_UniqueClaimsTable is a smoke test that verifies migration 000003
// creates the unique_claims table and its unique index (unique_claims_uq).
// It also checks RLS is enabled and the tenant_isolation_unique_claims policy
// exists — mirroring the assertions in TestRLS_PoliciesExist for this table.
func TestMigrate_UniqueClaimsTable(t *testing.T) {
	pool := newTestPool(t)

	// Reset and migrate.
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	ctx := context.Background()

	// Assert: table exists.
	var tableExists bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'unique_claims' AND table_schema = 'public')",
	).Scan(&tableExists)
	if err != nil {
		t.Fatalf("query information_schema.tables: %v", err)
	}
	if !tableExists {
		t.Fatal("unique_claims table does not exist after migration 000003")
	}

	// Assert: unique index exists.
	var indexExists bool
	err = pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'unique_claims' AND indexname = 'unique_claims_uq')",
	).Scan(&indexExists)
	if err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if !indexExists {
		t.Fatal("unique_claims_uq index does not exist after migration 000003")
	}

	// Assert: RLS enabled.
	var rlsEnabled bool
	err = pool.QueryRow(ctx,
		"SELECT relrowsecurity FROM pg_class WHERE relname = 'unique_claims'",
	).Scan(&rlsEnabled)
	if err != nil {
		t.Fatalf("query pg_class for RLS: %v", err)
	}
	if !rlsEnabled {
		t.Error("RLS not enabled on unique_claims")
	}

	// Assert: tenant isolation policy exists.
	var policyCount int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_policies WHERE tablename = 'unique_claims' AND policyname = 'tenant_isolation_unique_claims'",
	).Scan(&policyCount)
	if err != nil {
		t.Fatalf("query pg_policies: %v", err)
	}
	if policyCount == 0 {
		t.Error("tenant_isolation_unique_claims policy not found on unique_claims")
	}
}
