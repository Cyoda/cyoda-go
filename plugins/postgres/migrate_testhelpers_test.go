package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dropSchema drops all application tables and the migration tracking table by
// running DROP SCHEMA CASCADE + CREATE SCHEMA. This is faster and more robust
// than MigrateDown for test cleanup because MigrateDown can fail when test
// data violates constraints introduced by a DOWN migration (e.g. duplicate
// primary-key values across tenants when reverting a composite-PK migration).
//
// dropSchema lives in a _test.go file so production binaries cannot call it,
// even by mistake (e.g. a misconfigured CYODA_TEST_DB_URL pointing at a
// production database). DropSchemaForTest in export_test.go exposes it to the
// external test packages.
//
// Before dropping the schema, dropSchema terminates all OTHER backends
// connected to the same database. This is necessary because DROP SCHEMA
// CASCADE requires an AccessExclusive lock on every object it drops, and idle
// connections from the main conformance pool can hold those locks open
// (preventing the DROP from proceeding) even after the pool's Close() has
// been called but before puddle has fully drained its wait-group.
func dropSchema(pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		return fmt.Errorf("acquire connection for dropSchema: %w", err)
	}
	defer conn.Release()

	// Terminate other backends so DROP SCHEMA can acquire its exclusive locks
	// without waiting. This is safe in test environments.
	_, _ = conn.Exec(context.Background(),
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND datname = current_database()")

	if _, err := conn.Exec(context.Background(),
		"DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public"); err != nil {
		return fmt.Errorf("dropSchema: %w", err)
	}
	return nil
}
