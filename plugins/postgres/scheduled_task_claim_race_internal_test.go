package postgres

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestLostClaimRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"sibling claimed", &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "scheduled_tasks_one_running_per_entity_uq"}, true},
		{"crossed waits", fmt.Errorf("claim: %w", &pgconn.PgError{Code: pgerrcode.DeadlockDetected}), true},
		{"rival stalled past lock_timeout", &pgconn.PgError{Code: pgerrcode.LockNotAvailable}, true},
		{"another unique index", &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "scheduled_task_marks_pkey"}, false},
		{"serialization failure", &pgconn.PgError{Code: pgerrcode.SerializationFailure}, false},
		{"no server answer", errors.New("connection refused"), false},
		{"nil", nil, false},
	} {
		if got := lostClaimRace(tc.err); got != tc.want {
			t.Errorf("%s: lostClaimRace = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A lost race on the one-RUNNING-task-per-entity index is expected and logged
// at DEBUG. A lock wait or deadlock anywhere else in the claim is not, and is
// logged at WARN.
func TestLostClaimRaceLevel(t *testing.T) {
	lockTimeout := &pgconn.PgError{Code: pgerrcode.LockNotAvailable}
	for _, tc := range []struct {
		name string
		err  error
		want slog.Level
	}{
		{"index wait in the claim statement", claimStepError(lockTimeout), slog.LevelDebug},
		{"index refusal in the claim statement", claimStepError(&pgconn.PgError{Code: pgerrcode.UniqueViolation,
			ConstraintName: "scheduled_tasks_one_running_per_entity_uq"}), slog.LevelDebug},
		{"lock wait outside the claim statement", lockTimeout, slog.LevelWarn},
		{"deadlock outside the claim statement", &pgconn.PgError{Code: pgerrcode.DeadlockDetected}, slog.LevelWarn},
	} {
		if got := lostClaimRaceLevel(tc.err); got != tc.want {
			t.Errorf("%s: level = %v, want %v", tc.name, got, tc.want)
		}
	}
}
