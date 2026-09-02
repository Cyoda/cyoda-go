package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// openDBForCompat opens a *sql.DB against the test database for schema-compat
// tests. Each call returns a fresh connection; the caller must close it.
func openDBForCompat(t *testing.T) *sql.DB {
	t.Helper()
	poolCfg, err := pgxpool.ParseConfig(testDBURL(t))
	if err != nil {
		t.Fatalf("parse CYODA_TEST_DB_URL: %v", err)
	}

	db := stdlib.OpenDB(*poolCfg.ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// writeMigrationVersion seeds schema_migrations with a given version to
// simulate "newer than code" or "dirty" scenarios without running real
// migrations up to that point.
func writeMigrationVersion(t *testing.T, db *sql.DB, version int, dirty bool) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`, version, dirty); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSchemaCompat_SchemaNewerThanCode_Postgres(t *testing.T) {
	db := openDBForCompat(t)
	// Reset the schema_migrations table to a clean state first.
	_, _ = db.Exec(`DROP TABLE IF EXISTS schema_migrations`)
	writeMigrationVersion(t, db, 999, false)
	err := checkSchemaCompat(context.Background(), db, true /* autoMigrate */)
	if err == nil {
		t.Fatal("expected error when DB version is newer than code")
	}
}

func TestCheckSchemaCompat_SchemaOlder_NoAutoMigrate_Postgres(t *testing.T) {
	db := openDBForCompat(t)
	// Fresh DB — drop schema_migrations so there is no version recorded.
	_, _ = db.Exec(`DROP TABLE IF EXISTS schema_migrations`)
	err := checkSchemaCompat(context.Background(), db, false /* autoMigrate */)
	if err == nil {
		t.Fatal("expected error when autoMigrate=false and schema older than code")
	}
}

func TestCheckSchemaCompat_SchemaOlder_AutoMigrateTrue_Postgres(t *testing.T) {
	db := openDBForCompat(t)
	// Fresh DB — no schema_migrations table.
	_, _ = db.Exec(`DROP TABLE IF EXISTS schema_migrations`)
	err := checkSchemaCompat(context.Background(), db, true /* autoMigrate */)
	if err != nil {
		t.Fatalf("expected nil when autoMigrate=true and schema older than code; got %v", err)
	}
}

func TestCheckSchemaCompat_DirtyState_Postgres(t *testing.T) {
	db := openDBForCompat(t)
	_, _ = db.Exec(`DROP TABLE IF EXISTS schema_migrations`)
	writeMigrationVersion(t, db, 1, true /* dirty */)
	err := checkSchemaCompat(context.Background(), db, true)
	if err == nil {
		t.Fatal("expected error when migration state is dirty")
	}
}

func TestCheckSchemaCompat_SchemaMatches_Postgres(t *testing.T) {
	db := openDBForCompat(t)
	// Start from a clean state.
	_, _ = db.Exec(`DROP SCHEMA IF EXISTS public CASCADE`)
	_, _ = db.Exec(`CREATE SCHEMA public`)

	// runMigrations takes a *pgxpool.Pool, so we use a dedicated pool here.
	url := testDBURL(t)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse URL for migration pool: %v", err)
	}
	poolCfg.MaxConns = 2
	poolCfg.MinConns = 0
	poolCfg.HealthCheckPeriod = 24 * time.Hour // long period to avoid background health-check racing pool.Close
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("create migration pool: %v", err)
	}
	defer pool.Close()

	if err := runMigrations(context.Background(), pool, defaultMigrateLockTimeout); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	// After migrations, DB is at max version — compat check should pass with
	// autoMigrate=false since no migration is needed.
	err = checkSchemaCompat(context.Background(), db, false)
	if err != nil {
		t.Fatalf("expected nil when schema matches; got %v", err)
	}
}
