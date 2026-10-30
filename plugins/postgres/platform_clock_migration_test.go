package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// stampFunctionArgs returns the argument lists of every function named name
// that the database holds, in any schema.
func stampFunctionArgs(t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT pg_get_function_arguments(oid) FROM pg_proc WHERE proname = $1 ORDER BY 1`, name)
	if err != nil {
		t.Fatalf("read %s overloads: %v", name, err)
	}
	args, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s overloads: %v", name, err)
	}
	return args
}

// columnDefault reports whether table.column exists and its default
// expression ("" when it has none).
func columnDefault(t *testing.T, pool *pgxpool.Pool, table, column string) (exists bool, def string) {
	t.Helper()
	var d *string
	err := pool.QueryRow(context.Background(), `SELECT column_default FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, table, column).Scan(&d)
	if err == pgx.ErrNoRows {
		return false, ""
	}
	if err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	if d == nil {
		return true, ""
	}
	return true, *d
}

func requireArgs(t *testing.T, pool *pgxpool.Pool, name string, want string) {
	t.Helper()
	got := stampFunctionArgs(t, pool, name)
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s overloads = %q, want exactly one: %q", name, got, want)
	}
}

// Migration 17 replaces each stamp function with one that takes an optional
// time, leaving exactly one function of each name, and drops two dead
// defaults. Down restores version 16's functions, the default and the column.
func TestMigration17_PlatformClock(t *testing.T) {
	pool := newCTPool(t, freshCTDatabase(t), 4, nil)
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	requireArgs(t, pool, "cyoda_stamp", "tenant_key integer, at_us bigint DEFAULT NULL::bigint")
	requireArgs(t, pool, "cyoda_consistency_time", "tenant_key integer, wait_budget_ms bigint, at_us bigint DEFAULT NULL::bigint")
	if exists, def := columnDefault(t, pool, "search_jobs", "created_at"); !exists || def != "" {
		t.Errorf("search_jobs.created_at: exists %v, default %q; want it to exist with no default", exists, def)
	}
	if exists, _ := columnDefault(t, pool, "model_schema_extensions", "created_at"); exists {
		t.Error("model_schema_extensions.created_at still exists")
	}

	if err := postgres.MigrateToVersionForTest(pool, 16); err != nil {
		t.Fatalf("migrate down to 16: %v", err)
	}
	requireArgs(t, pool, "cyoda_stamp", "tenant_key integer")
	requireArgs(t, pool, "cyoda_consistency_time", "tenant_key integer, wait_budget_ms bigint")
	if exists, def := columnDefault(t, pool, "search_jobs", "created_at"); !exists || def != "now()" {
		t.Errorf("search_jobs.created_at after down: exists %v, default %q; want now()", exists, def)
	}
	if exists, def := columnDefault(t, pool, "model_schema_extensions", "created_at"); !exists || def != "CURRENT_TIMESTAMP" {
		t.Errorf("model_schema_extensions.created_at after down: exists %v, default %q; want CURRENT_TIMESTAMP", exists, def)
	}
}
