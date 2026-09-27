package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func migrateTo(t *testing.T, db *sql.DB, version uint) {
	t.Helper()
	driver, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{NoTxWrap: true})
	if err != nil {
		t.Fatalf("migration driver: %v", err)
	}
	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

// A task pending before migration 9 stays pending after it, as a new life.
// The row is written the way version 8 wrote it, without armed_by columns.
func TestMigration9_KeepsPendingTasksAsNewLives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m9.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	migrateTo(t, db, 8)
	if _, err := db.Exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, attempt_count, redispatch_after)
		VALUES ('legacy:S:T', 'tenant-A', ?, 1000, 'legacy', 'M', 1, 'T', 'S', 0, 2, 5000)`,
		string(spi.ScheduledTaskFireTransition)); err != nil {
		t.Fatalf("insert version-8 row: %v", err)
	}
	migrateTo(t, db, 9)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := NewStoreFactoryForTest(context.Background(), path)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	got, found, err := sts.Get(context.Background(), "tenant-A", "legacy:S:T")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v; want the migrated row", found, err)
	}
	if got.Status != spi.ScheduledTaskWaiting || got.ArmToken == uuid.Nil || got.NextAttemptTime != 1000 ||
		got.Attempts != 0 || got.Claim != nil || got.ArmedBy != (spi.Principal{}) {
		t.Fatalf("migrated row = %+v, want WAITING, a new arm token, due at 1000, no attempts, no claim, zero ArmedBy", got)
	}
}

func TestMigration9_Down(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "m9down.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	migrateTo(t, db, 9)
	if _, err := db.Exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, arm_token, status, next_attempt_time)
		VALUES ('e1:S:T', 'tenant-A', 'FIRE_TRANSITION', 1000, 'e1', 'M', 1, 'T', 'S', 0, ?, 'WAITING', 1000)`,
		uuid.NewString()); err != nil {
		t.Fatalf("insert version-9 row: %v", err)
	}
	migrateTo(t, db, 8)
	var attempts int
	if err := db.QueryRow(`SELECT attempt_count FROM scheduled_tasks WHERE id = 'e1:S:T'`).Scan(&attempts); err != nil {
		t.Fatalf("read version-8 row: %v", err)
	}
	for _, table := range []string{"scheduled_task_marks", "scheduler_owners"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after down: count %d, err %v; want dropped", table, n, err)
		}
	}
}
