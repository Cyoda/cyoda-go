package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A lock wait or deadlock a claim meets is returned, classified: ErrTaskBusy
// for a lock wait, ErrConflict for a deadlock. The SQLSTATE stays in the
// chain.
func TestClaimError(t *testing.T) {
	wait := &pgconn.PgError{Code: pgerrcode.LockNotAvailable}
	if err := claimError(fmt.Errorf("rank: %w", wait)); !errors.Is(err, spi.ErrTaskBusy) || !errors.As(err, new(*pgconn.PgError)) {
		t.Errorf("lock wait: err = %v, want ErrTaskBusy over 55P03", err)
	}
	deadlock := &pgconn.PgError{Code: pgerrcode.DeadlockDetected}
	if err := claimError(deadlock); !errors.Is(err, spi.ErrConflict) || errors.Is(err, spi.ErrTaskBusy) {
		t.Errorf("deadlock: err = %v, want ErrConflict", err)
	}
	if err := claimError(classifyError(deadlock)); !errors.Is(err, spi.ErrConflict) {
		t.Errorf("classified deadlock: err = %v, want ErrConflict", err)
	}
	other := errors.New("connection refused")
	if err := claimError(other); !errors.Is(err, other) || errors.Is(err, spi.ErrTaskBusy) || errors.Is(err, spi.ErrConflict) {
		t.Errorf("other: err = %v, want it unmarked", err)
	}
}
