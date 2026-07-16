package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := skipIfNoPostgres(t)

	// Use pgxpool.ParseConfig + NewWithConfig so we can set HealthCheckPeriod
	// to 24h (effectively disabled). The default HealthCheckPeriod triggers a
	// backgroundHealthCheck goroutine that races with pool.Close() in
	// pgx v5.9.1, causing pool.Close to block in puddle's allResourcesWG.Wait
	// and hang the test binary.
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("failed to parse pool config: %v", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 0 // 0 avoids keeping idle connections that the health check probes
	poolCfg.MaxConnIdleTime = 60 * time.Second
	poolCfg.HealthCheckPeriod = 24 * time.Hour

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrate_AppliesSchema(t *testing.T) {
	pool := newTestPool(t)

	// Clean slate — use DropSchema (robust) instead of MigrateDown (fragile
	// when test data violates DOWN-migration constraints).
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify tables exist
	tables := []string{"entities", "entity_versions", "sm_audit_events", "models", "kv_store", "messages", "search_jobs", "search_job_results", "scheduled_tasks"}
	for _, table := range tables {
		var exists bool
		err := pool.QueryRow(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)", table).Scan(&exists)
		if err != nil {
			t.Fatalf("failed to check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist after migration", table)
		}
	}

	// Verify RLS is enabled but NOT forced (migration 000002 removes FORCE).
	// FORCE is deferred to Plan 5 when SET LOCAL is wired at transaction start.
	// Application-level WHERE tenant_id = $1 is the primary isolation mechanism.
	// Only check tables that have RLS configured (initial schema tables).
	rlsTables := []string{"entities", "entity_versions", "sm_audit_events", "models", "kv_store", "messages"}
	for _, table := range rlsTables {
		var rlsEnabled, rlsForced bool
		err := pool.QueryRow(context.Background(),
			"SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1", table).Scan(&rlsEnabled, &rlsForced)
		if err != nil {
			t.Fatalf("failed to check RLS for %s: %v", table, err)
		}
		if !rlsEnabled {
			t.Errorf("RLS not enabled on table %s", table)
		}
		if rlsForced {
			t.Errorf("FORCE RLS should NOT be set on table %s (deferred to Plan 5)", table)
		}
	}

	// Clean up
	if err := postgres.MigrateDownForTest(pool); err != nil {
		t.Fatalf("migration rollback failed: %v", err)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("first migration failed: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("second migration (idempotent) failed: %v", err)
	}

	_ = postgres.DropSchemaForTest(pool)
}
