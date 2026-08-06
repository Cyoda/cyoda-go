package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"

	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// runMigrations applies pending migrations. Uses m.GracefulStop to honor
// context cancellation at migration-step boundaries (golang-migrate's
// m.Up() itself takes no context).
func runMigrations(ctx context.Context, db *sql.DB) error {
	driver, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{
		NoTxWrap: true, // PRAGMAs and multi-statement DDL cannot run inside a transaction
	})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}

	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		err := m.Up()
		if errors.Is(err, migrate.ErrNoChange) {
			err = nil
		}
		done <- err
	}()

	select {
	case <-ctx.Done():
		select {
		case m.GracefulStop <- true:
		default:
		}
		<-done // wait for the goroutine to exit so we don't leak it
		return fmt.Errorf("sqlite migrate: %w", ctx.Err())
	case err := <-done:
		return err
	}
}

// checkSchemaCompat enforces the schema-compatibility contract on startup:
//   - schema newer than code → fatal, regardless of autoMigrate
//   - schema older than code, autoMigrate=false → fatal
//   - schema older than code, autoMigrate=true → caller proceeds with runMigrations
//   - schema matches → proceed
//   - dirty state → fatal, manual intervention required
func checkSchemaCompat(ctx context.Context, db *sql.DB, autoMigrate bool) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("schema compat: context cancelled: %w", err)
	}
	driver, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{NoTxWrap: true})
	if err != nil {
		return fmt.Errorf("schema compat: create driver: %w", err)
	}
	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("schema compat: open migration source: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("schema compat: create migrator: %w", err)
	}

	maxVersion, err := maxMigrationVersion(src)
	if err != nil {
		return fmt.Errorf("schema compat: scan embedded migrations: %w", err)
	}

	dbVersion, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		dbVersion = 0 // fresh database — treat as older-than-code
	case err != nil:
		return fmt.Errorf("schema compat: read DB version: %w", err)
	}
	// This refusal and the newer-than-binary one below are kept word for word in
	// step with the postgres plugin's dirtySchemaError and
	// schemaNewerThanBinaryError. An operator reads the refusal, not the plugin
	// that produced it, and the recovery procedure the pointer names covers this
	// backend as well — clearing the dirty flag is not something to be
	// reverse-engineered from a bare statement of the condition.
	if dirty {
		return fmt.Errorf("schema compat: database migration state is dirty at version %d — "+
			"manual intervention required; run `cyoda help cli migrate` for the recovery procedure", dbVersion)
	}

	switch {
	case dbVersion > maxVersion:
		return fmt.Errorf("schema compat: database schema version %d is newer than this binary's max migration version %d — refusing to start to avoid data corruption", dbVersion, maxVersion)
	case dbVersion < maxVersion && !autoMigrate:
		return fmt.Errorf("schema compat: database schema version %d is older than code (%d) and CYODA_SQLITE_AUTO_MIGRATE=false — set CYODA_SQLITE_AUTO_MIGRATE=true and restart, or apply migrations out-of-band", dbVersion, maxVersion)
	}
	return nil
}

// maxMigrationVersion walks the embedded migration source and returns
// the highest version present.
func maxMigrationVersion(src source.Driver) (uint, error) {
	v, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("first migration: %w", err)
	}
	max := v
	for {
		next, err := src.Next(max)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("next migration after %d: %w", max, err)
		}
		max = next
	}
	return max, nil
}
