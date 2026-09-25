package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// indexWait is where PostgreSQL reports a wait on another transaction's entry
// in the one-RUNNING-task-per-entity index (observed on PostgreSQL 17).
const indexWait = `while inserting index tuple (0,4) in relation "scheduled_tasks_one_running_per_entity_uq"`

// Only the sibling race on the one-RUNNING-task-per-entity index is
// swallowed: a refusal by the index, or a lock wait or deadlock that the claim
// statement met on the index.
func TestSiblingRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the index refused the claim", &pgconn.PgError{Code: pgerrcode.UniqueViolation,
			ConstraintName: "scheduled_tasks_one_running_per_entity_uq"}, true},
		{"the index refused the claim, already run through classifyError", classifyError(&pgconn.PgError{
			Code: pgerrcode.UniqueViolation, ConstraintName: "scheduled_tasks_one_running_per_entity_uq"}), true},
		{"the claim waited on the index past lock_timeout",
			claimStepError(&pgconn.PgError{Code: pgerrcode.LockNotAvailable, Where: indexWait}), true},
		{"the claim deadlocked on the index",
			claimStepError(&pgconn.PgError{Code: pgerrcode.DeadlockDetected, Where: indexWait}), true},
		{"the claim waited on something else",
			claimStepError(&pgconn.PgError{Code: pgerrcode.LockNotAvailable,
				Where: `while updating tuple (0,1) in relation "scheduled_tasks"`}), false},
		{"a lock wait outside the claim statement", &pgconn.PgError{Code: pgerrcode.LockNotAvailable, Where: indexWait}, false},
		{"a deadlock outside the claim statement", &pgconn.PgError{Code: pgerrcode.DeadlockDetected}, false},
		{"another unique index", &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "scheduled_task_marks_pkey"}, false},
		{"serialization failure", &pgconn.PgError{Code: pgerrcode.SerializationFailure}, false},
		{"no server answer", errors.New("connection refused"), false},
		{"nil", nil, false},
	} {
		if got := siblingRace(tc.err); got != tc.want {
			t.Errorf("%s: siblingRace = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Any other lock wait or deadlock is returned, classified: ErrTaskBusy for a
// lock wait, ErrConflict for a deadlock. The SQLSTATE stays in the chain.
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
