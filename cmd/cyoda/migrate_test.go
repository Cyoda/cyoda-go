package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// isolateEnvFiles makes a call to app.LoadEnvFiles hermetic: the user
// config, a ./.env and profile files are looked up in empty temporary
// directories, so the developer's own user config and env files are never
// read, and nothing from them is left set in the test process. The Linux
// system config (/etc/cyoda/cyoda.env) cannot be redirected, so a test that
// depends on a variable sets it itself. It returns the XDG_CONFIG_HOME
// directory, under which a test may write a user config.
func isolateEnvFiles(t *testing.T) string {
	t.Helper()
	xdg := setupIsolatedConfig(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CYODA_PROFILES", "")
	t.Chdir(t.TempDir())
	return xdg
}

// TestRunMigrate_ReadsEnvFiles pins that migrate reads the env files the
// server reads. Without them, a postgres backend configured in the user
// config or a profile file migrated nothing and reported success as a
// memory no-op.
func TestRunMigrate_ReadsEnvFiles(t *testing.T) {
	xdg := isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "") // restores the variable's absence at cleanup
	os.Unsetenv("CYODA_STORAGE_BACKEND")
	if err := os.MkdirAll(filepath.Join(xdg, "cyoda"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "cyoda", "cyoda.env"), []byte("CYODA_STORAGE_BACKEND=no-such-backend\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := runMigrate(nil); code != 1 {
		t.Errorf("runMigrate exit code = %d; want 1 (unknown backend from the user config)", code)
	}
}

// TestRunMigrate_MemoryBackendNoOp confirms the memory backend exits 0.
func TestRunMigrate_MemoryBackendNoOp(t *testing.T) {
	isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "memory")

	code := runMigrate(nil)
	if code != 0 {
		t.Errorf("memory backend migrate should exit 0; got %d", code)
	}
}

// TestRunMigrate_UnknownFlagRejected verifies argument parsing errors
// produce non-zero exit.
func TestRunMigrate_UnknownFlagRejected(t *testing.T) {
	isolateEnvFiles(t)
	code := runMigrate([]string{"--notaflag"})
	if code == 0 {
		t.Error("unknown flag should cause non-zero exit")
	}
}

// TestRunMigrate_TimeoutFlagParsed verifies --timeout is honored.
func TestRunMigrate_TimeoutFlagParsed(t *testing.T) {
	isolateEnvFiles(t)
	cfg, err := parseMigrateArgs([]string{"--timeout", "10m"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.Timeout.String() != "10m0s" {
		t.Errorf("want timeout 10m, got %s", cfg.Timeout)
	}
}

// TestRunMigrate_MissingPostgresDSN confirms a clear error when the
// postgres backend is selected but no DSN is provided.
func TestRunMigrate_MissingPostgresDSN(t *testing.T) {
	isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "postgres")
	t.Setenv("CYODA_POSTGRES_URL", "")
	t.Setenv("CYODA_POSTGRES_URL_FILE", "")

	code := runMigrate(nil)
	if code == 0 {
		t.Error("missing DSN should cause non-zero exit")
	}
}

// TestRunMigrate_SQLiteBackendNoOp confirms the sqlite backend exits 0.
func TestRunMigrate_SQLiteBackendNoOp(t *testing.T) {
	isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "sqlite")

	code := runMigrate(nil)
	if code != 0 {
		t.Errorf("sqlite backend migrate should exit 0; got %d", code)
	}
}

// TestRunMigrate_UnknownBackend confirms an unknown backend exits non-zero.
func TestRunMigrate_UnknownBackend(t *testing.T) {
	isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "cassandra")

	code := runMigrate(nil)
	if code == 0 {
		t.Error("unknown backend should cause non-zero exit")
	}
}

// TestRunMigrate_PostgresTimeoutPath verifies the context.DeadlineExceeded
// branch exits 1 without requiring Docker.
func TestRunMigrate_PostgresTimeoutPath(t *testing.T) {
	isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "postgres")
	t.Setenv("CYODA_POSTGRES_URL", "postgres://fake@localhost:1/fake")

	orig := pgMigrate
	t.Cleanup(func() { pgMigrate = orig })
	pgMigrate = func(ctx context.Context, dsn string) error {
		return context.DeadlineExceeded
	}

	code := runMigrate(nil)
	if code != 1 {
		t.Errorf("timeout should exit 1; got %d", code)
	}
}

// TestRunMigrate_PostgresGenericError verifies a non-timeout migration error
// exits 1, keeping the generic-error branch distinct from the timeout branch.
func TestRunMigrate_PostgresGenericError(t *testing.T) {
	isolateEnvFiles(t)
	t.Setenv("CYODA_STORAGE_BACKEND", "postgres")
	t.Setenv("CYODA_POSTGRES_URL", "postgres://fake@localhost:1/fake")

	orig := pgMigrate
	t.Cleanup(func() { pgMigrate = orig })
	pgMigrate = func(ctx context.Context, dsn string) error {
		return errors.New("fake db error")
	}

	code := runMigrate(nil)
	if code != 1 {
		t.Errorf("generic error should exit 1; got %d", code)
	}
}

// TestRunMigrate_IntegrationPostgres covers end-to-end: first call
// applies migrations; second call is idempotent; schema-newer-than-code
// refuses. Requires Docker (testcontainers).
func TestRunMigrate_IntegrationPostgres(t *testing.T) {
	isolateEnvFiles(t)
	if testing.Short() {
		t.Skip("integration test; run without -short")
	}

	dsn := startTestPostgres(t)

	// First run: applies migrations, exits 0.
	t.Setenv("CYODA_STORAGE_BACKEND", "postgres")
	t.Setenv("CYODA_POSTGRES_URL", dsn)
	t.Setenv("CYODA_POSTGRES_URL_FILE", "")

	code := runMigrate(nil)
	if code != 0 {
		t.Fatalf("first migrate run should succeed; got exit code %d", code)
	}

	// Second run: idempotent, exits 0.
	code = runMigrate(nil)
	if code != 0 {
		t.Fatalf("second migrate run (idempotent) should succeed; got exit code %d", code)
	}

	// Schema-newer-than-code: inject a large version number, expect refusal.
	advanceSchemaVersion(t, dsn, 999999)
	code = runMigrate(nil)
	if code == 0 {
		t.Error("migrate with schema newer than code should exit non-zero")
	}
}

// startTestPostgres boots a Postgres testcontainer and returns its DSN.
// It registers cleanup on the test.
func startTestPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("cyoda_migrate_test"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		_ = pgContainer.Terminate(ctx)
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}
	return dsn
}

// advanceSchemaVersion sets the schema_migrations table to a given version
// to simulate "DB schema newer than code" scenarios.
func advanceSchemaVersion(t *testing.T, dsn string, version int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM schema_migrations; INSERT INTO schema_migrations (version, dirty) VALUES (%d, false)`,
		version,
	))
	if err != nil {
		t.Fatalf("advance schema version: %v", err)
	}
}
