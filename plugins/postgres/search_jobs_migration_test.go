package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func pointInTimeNullable(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var nullable string
	if err := pool.QueryRow(context.Background(), `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'search_jobs' AND column_name = 'point_in_time'`).Scan(&nullable); err != nil {
		t.Fatalf("read search_jobs.point_in_time nullability: %v", err)
	}
	return nullable
}

func insertJobWithoutPointInTime(pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(context.Background(), `INSERT INTO search_jobs
		(id, tenant_id, status, model_name, model_ver, point_in_time)
		VALUES ($1, 'tenant-A', 'RUNNING', 'M', '1', NULL)`, id)
	return err
}

// Every search job has a point in time: the database refuses a row without one.
func TestMigration15_SearchJobPointInTimeIsNotNull(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	if got := pointInTimeNullable(t, pool); got != "NO" {
		t.Errorf("search_jobs.point_in_time is_nullable = %q, want NO", got)
	}

	err := insertJobWithoutPointInTime(pool, "j-null")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.NotNullViolation || pgErr.ColumnName != "point_in_time" {
		t.Fatalf("insert with NULL point_in_time: err = %v, want a not-null violation on point_in_time", err)
	}
}

func TestMigration15_Down(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	if err := postgres.MigrateToVersionForTest(pool, 15); err != nil {
		t.Fatalf("migrate to 15: %v", err)
	}
	if err := postgres.MigrateToVersionForTest(pool, 14); err != nil {
		t.Fatalf("migrate down to 14: %v", err)
	}

	if got := pointInTimeNullable(t, pool); got != "YES" {
		t.Errorf("search_jobs.point_in_time is_nullable after down = %q, want YES", got)
	}
	if err := insertJobWithoutPointInTime(pool, "j-null"); err != nil {
		t.Fatalf("insert with NULL point_in_time at version 14: %v", err)
	}
}
