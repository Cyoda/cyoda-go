package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	sqlite3 "github.com/ncruces/go-sqlite3"
)

// classifyClaimError maps SQLite errors from unique-claim INSERT operations to
// spi.ErrUniqueViolation. It is used ONLY around claim-INSERT calls so that
// entity-PK and other constraint violations are NOT mapped to ErrUniqueViolation.
//
// Contrast with classifyError, which maps CONSTRAINT_UNIQUE → spi.ErrConflict
// (retryable) for the entity write path.
func classifyClaimError(err error) error {
	if err == nil {
		return nil
	}
	var xcode sqlite3.ExtendedErrorCode
	if errors.As(err, &xcode) && xcode == sqlite3.CONSTRAINT_UNIQUE {
		return fmt.Errorf("%w", spi.ErrUniqueViolation)
	}
	return err
}

// classifyError maps SQLite errors to SPI-level sentinel errors so the
// handler layer can react uniformly across storage backends.
//
// Mappings:
//   - SQLITE_BUSY (database locked) → spi.ErrConflict — retryable
//   - CONSTRAINT_UNIQUE / CONSTRAINT_PRIMARYKEY → spi.ErrConflict — retryable
//   - sql.ErrNoRows → spi.ErrNotFound
//
// The original SQLite error stays in the chain via fmt.Errorf %w so
// observability can type-assert via errors.As.
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	// sql.ErrNoRows → not found.
	if errors.Is(err, sql.ErrNoRows) {
		return spi.ErrNotFound
	}

	// SQLITE_BUSY → conflict (retryable).
	if errors.Is(err, sqlite3.BUSY) {
		return fmt.Errorf("%w: %w", spi.ErrConflict, err)
	}

	// CONSTRAINT_UNIQUE or CONSTRAINT_PRIMARYKEY → conflict (retryable).
	var xcode sqlite3.ExtendedErrorCode
	if errors.As(err, &xcode) {
		switch xcode {
		case sqlite3.CONSTRAINT_UNIQUE, sqlite3.CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", spi.ErrConflict, err)
		}
	}

	// CONSTRAINT (generic, without extended code match) — not retryable,
	// pass through as-is. Only unique/pk violations are conflicts.

	return err
}
