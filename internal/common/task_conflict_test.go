package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// countingOp returns an op that answers errs in order, then nil, and a
// pointer to how many times it ran.
func countingOp(errs ...error) (func() error, *int) {
	calls := 0
	return func() error {
		calls++
		if calls <= len(errs) {
			return errs[calls-1]
		}
		return nil
	}, &calls
}

func conflict() error { return fmt.Errorf("task row changed: %w", spi.ErrConflict) }

func TestRetryOnTaskConflict_SuccessRunsOnce(t *testing.T) {
	op, calls := countingOp()
	if err := RetryOnTaskConflict(context.Background(), true, op); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}

func TestRetryOnTaskConflict_ConflictThenSuccess(t *testing.T) {
	op, calls := countingOp(conflict())
	if err := RetryOnTaskConflict(context.Background(), true, op); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2", *calls)
	}
}

func TestRetryOnTaskConflict_PersistentConflict_RunsOncePlusRetries(t *testing.T) {
	errs := make([]error, 10)
	for i := range errs {
		errs[i] = conflict()
	}
	op, calls := countingOp(errs...)
	err := RetryOnTaskConflict(context.Background(), true, op)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want the last conflict", err)
	}
	if want := 1 + TaskConflictRetries; *calls != want {
		t.Errorf("calls = %d, want %d", *calls, want)
	}
}

func TestRetryOnTaskConflict_ConflictCarriedByAppError(t *testing.T) {
	appErr := Operational(http.StatusConflict, ErrCodeConflict, "transaction conflict — retry").AsRetryable().WithCause(conflict())
	op, calls := countingOp(appErr)
	if err := RetryOnTaskConflict(context.Background(), true, op); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2 (an AppError whose cause is a conflict is retried)", *calls)
	}
}

func TestRetryOnTaskConflict_OtherErrorNotRetried(t *testing.T) {
	boom := errors.New("storage down")
	op, calls := countingOp(boom, boom)
	if err := RetryOnTaskConflict(context.Background(), true, op); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}

func TestRetryOnTaskConflict_JoinedRunsOnce(t *testing.T) {
	op, calls := countingOp(conflict(), conflict())
	err := RetryOnTaskConflict(context.Background(), false, op)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want the conflict", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1: a joined op's conflict belongs to its owner", *calls)
	}
}

func TestRetryOnTaskConflict_StopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op, calls := countingOp(conflict(), conflict())
	if err := RetryOnTaskConflict(ctx, true, op); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want the conflict", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want 1", *calls)
	}
}
