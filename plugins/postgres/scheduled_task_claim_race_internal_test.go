package postgres

import (
	"errors"
	"fmt"
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
