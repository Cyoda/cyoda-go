package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func openMemorySQLiteForCompat(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// writeMigrationVersion seeds schema_migrations with a given version so we
// can simulate "newer than code" and "dirty" scenarios without actually
// running migrations up to that point.
func writeMigrationVersion(t *testing.T, db *sql.DB, version int, dirty bool) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version uint64, dirty bool)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	d := 0
	if dirty {
		d = 1
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (?, ?)`, version, d); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSchemaCompat_SchemaNewerThanCode(t *testing.T) {
	db := openMemorySQLiteForCompat(t)
	writeMigrationVersion(t, db, 999, false)
	err := checkSchemaCompat(context.Background(), db, true /* autoMigrate */)
	if err == nil {
		t.Fatal("expected error when DB version is newer than code")
	}
}

func TestCheckSchemaCompat_SchemaOlder_NoAutoMigrate(t *testing.T) {
	db := openMemorySQLiteForCompat(t)
	// Fresh DB — no schema_migrations table.
	err := checkSchemaCompat(context.Background(), db, false /* autoMigrate */)
	if err == nil {
		t.Fatal("expected error when autoMigrate=false and schema older than code")
	}
}

func TestCheckSchemaCompat_SchemaOlder_AutoMigrateTrue(t *testing.T) {
	db := openMemorySQLiteForCompat(t)
	err := checkSchemaCompat(context.Background(), db, true /* autoMigrate */)
	if err != nil {
		t.Fatalf("expected nil when autoMigrate=true and schema older than code; got %v", err)
	}
}

func TestCheckSchemaCompat_DirtyState(t *testing.T) {
	db := openMemorySQLiteForCompat(t)
	writeMigrationVersion(t, db, 1, true /* dirty */)
	err := checkSchemaCompat(context.Background(), db, true)
	if err == nil {
		t.Fatal("expected error when migration state is dirty")
	}
}

// TestCheckSchemaCompat_RefusalsCarryTheSameGuidanceAsPostgres — an operator
// meets a refusal to start without first deciding which storage plugin wrote
// it, so both plugins say the same thing. The dirty message in particular has
// to carry the pointer to the recovery procedure: clearing the flag is not
// something to be reverse-engineered from a bare statement of the condition,
// and the procedure the topic documents covers this backend too.
func TestCheckSchemaCompat_RefusalsCarryTheSameGuidanceAsPostgres(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		db := openMemorySQLiteForCompat(t)
		writeMigrationVersion(t, db, 1, true /* dirty */)
		err := checkSchemaCompat(context.Background(), db, true)
		if err == nil {
			t.Fatal("expected a refusal on a dirty schema")
		}
		for _, want := range []string{
			"database migration state is dirty at version 1",
			"manual intervention required",
			"cyoda help cli migrate",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q is missing %q", err, want)
			}
		}
	})

	t.Run("newer than binary", func(t *testing.T) {
		db := openMemorySQLiteForCompat(t)
		writeMigrationVersion(t, db, 999, false)
		err := checkSchemaCompat(context.Background(), db, true)
		if err == nil {
			t.Fatal("expected a refusal on a schema newer than the binary")
		}
		for _, want := range []string{
			"database schema version 999 is newer than this binary's max migration version",
			"refusing to start to avoid data corruption",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q is missing %q", err, want)
			}
		}
	})
}

func TestCheckSchemaCompat_SchemaMatches(t *testing.T) {
	db := openMemorySQLiteForCompat(t)
	if err := runMigrations(context.Background(), db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	// After migrations, DB is at max version — compat check should pass with
	// autoMigrate=false since no migration is needed.
	err := checkSchemaCompat(context.Background(), db, false)
	if err != nil {
		t.Fatalf("expected nil when schema matches; got %v", err)
	}
}
