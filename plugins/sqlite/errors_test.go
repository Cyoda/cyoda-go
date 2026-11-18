package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	sqlite3 "github.com/ncruces/go-sqlite3"
)

func TestClassifyError_NilPassesThrough(t *testing.T) {
	if err := classifyError(nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestClassifyError_ErrNoRows_MapsToNotFound(t *testing.T) {
	err := classifyError(sql.ErrNoRows)
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestClassifyError_WrappedErrNoRows_MapsToNotFound(t *testing.T) {
	wrapped := fmt.Errorf("scan entity: %w", sql.ErrNoRows)
	err := classifyError(wrapped)
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestClassifyError_Busy_MapsToConflict(t *testing.T) {
	// sqlite3.BUSY is an ErrorCode that implements error; errors.Is
	// matches it through the *sqlite3.Error.Is method.
	busyErr := sqlite3.BUSY
	err := classifyError(busyErr)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	// The original BUSY should still be in the chain.
	if !errors.Is(err, sqlite3.BUSY) {
		t.Fatalf("expected BUSY to be in the chain, got %v", err)
	}
}

func TestClassifyError_ConstraintUnique_MapsToConflict(t *testing.T) {
	err := classifyError(sqlite3.CONSTRAINT_UNIQUE)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestClassifyError_ConstraintPrimaryKey_MapsToConflict(t *testing.T) {
	err := classifyError(sqlite3.CONSTRAINT_PRIMARYKEY)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestClassifyError_OtherError_PassesThrough(t *testing.T) {
	other := fmt.Errorf("something else went wrong")
	err := classifyError(other)
	if err != other {
		t.Fatalf("expected original error, got %v", err)
	}
}
