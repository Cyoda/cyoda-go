package common

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestValidateRequestTimeoutMillis(t *testing.T) {
	for _, tc := range []struct {
		millis int64
		wantOK bool
	}{{1, true}, {60000, true}, {0, false}, {-5, false}, {math.MaxInt64 / int64(time.Millisecond) + 1, false}} {
		err := ValidateRequestTimeoutMillis(tc.millis)
		if tc.wantOK && err != nil {
			t.Errorf("millis=%d: unexpected %v", tc.millis, err)
		}
		if !tc.wantOK {
			if err == nil {
				t.Fatalf("millis=%d: want 400", tc.millis)
			}
			if err.Status != http.StatusBadRequest || err.Code != ErrCodeBadRequest {
				t.Errorf("millis=%d: got status=%d code=%s", tc.millis, err.Status, err.Code)
			}
		}
	}
}

func TestClassifyRequestTimeout_OursExpired(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	err := fmt.Errorf("failed to commit transaction: %w", ctx.Err())
	appErr := ClassifyRequestTimeout(ctx, err, ErrCodeTransactionTimeout)
	if appErr == nil {
		t.Fatal("want 408, got nil")
	}
	if appErr.Status != http.StatusRequestTimeout || appErr.Code != ErrCodeTransactionTimeout || !appErr.Retryable {
		t.Errorf("got %+v", appErr)
	}
}

func TestClassifyRequestTimeout_UnwrapsAppErrorCause(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	wrapped := Internal("failed to commit transaction", fmt.Errorf("segment: %w", context.DeadlineExceeded))
	if got := ClassifyRequestTimeout(ctx, wrapped, ErrCodeTransactionTimeout); got == nil {
		t.Fatal("must unwrap AppError cause chain")
	}
}

func TestClassifyRequestTimeout_NeverFromCtxStateAlone(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	// Deadline HAS expired, but the error is an unrelated conflict — must pass through (nil).
	if got := ClassifyRequestTimeout(ctx, errors.New("transaction conflict"), ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("ctx-state-alone classification forbidden, got %v", got)
	}
}

func TestClassifyRequestTimeout_NoMarkerNo408(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := ClassifyRequestTimeout(ctx, ctx.Err(), ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("foreign deadline must not classify as 408, got %v", got)
	}
}

func TestClassifyRequestTimeout_CanceledIsNot408(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 60000)
	cancel() // client disconnect, not deadline
	if got := ClassifyRequestTimeout(ctx, ctx.Err(), ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("context.Canceled must not be 408, got %v", got)
	}
}

// TestClassifyRequestTimeout_MarkerNotYetExpired pins the ours-actually-
// expired conjunct: the marker is present and the chain contains
// context.DeadlineExceeded, but ctx itself has NOT expired (e.g. a nested
// deadline elsewhere — a postgres pool-acquire timeout — bubbled up while
// the client's own budget is still live). Must not classify as 408: the
// marker + chain alone is not sufficient, ctx must also actually be expired.
func TestClassifyRequestTimeout_MarkerNotYetExpired(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 60000)
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("precondition: ctx must still be live")
	}
	err := fmt.Errorf("nested pool-acquire timeout: %w", context.DeadlineExceeded)
	if got := ClassifyRequestTimeout(ctx, err, ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("a foreign DeadlineExceeded while our ctx is still live must not be 408, got %v", got)
	}
}

// TestClassifyRequestTimeout_CommitInterrupted_NeverClassifies pins that an
// error carrying ErrCommitInterrupted is disqualified from 408 even when the
// marker is present, the chain contains context.DeadlineExceeded, AND ctx
// has actually expired (the overlap case: the client's own deadline also
// happened to fire around the same time as the commit's own interruption).
// The commit's outcome is in-doubt — it must never present as the client's
// clean "nothing was committed" 408.
func TestClassifyRequestTimeout_CommitInterrupted_NeverClassifies(t *testing.T) {
	ctx, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()
	err := fmt.Errorf("%w: %w", ErrCommitInterrupted, fmt.Errorf("commit: %w", context.DeadlineExceeded))
	if got := ClassifyRequestTimeout(ctx, err, ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("a commit-interrupted error must never classify as 408, got %v", got)
	}
}

// TestWrapIfCommitInterrupted_WrapsOnlyWhenCommitCtxItselfFailed pins
// WrapIfCommitInterrupted's exact condition, tested directly (fast — no
// 30s commitBudget wait) rather than through the real 30s-budgeted
// CommitContext: it wraps ONLY when both err != nil AND the given
// commitCtx itself shows an error, and never mutates a nil err.
func TestWrapIfCommitInterrupted_WrapsOnlyWhenCommitCtxItselfFailed(t *testing.T) {
	liveCtx := context.Background()
	expiredCtx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("live commitCtx, clean failure — unwrapped", func(t *testing.T) {
		orig := errConflictStandIn
		got := WrapIfCommitInterrupted(liveCtx, orig)
		if !errors.Is(got, orig) {
			t.Fatalf("expected the original error preserved via errors.Is, got %v", got)
		}
		if errors.Is(got, ErrCommitInterrupted) {
			t.Fatalf("must not wrap when commitCtx is still live: %v", got)
		}
	})

	t.Run("interrupted commitCtx — wrapped", func(t *testing.T) {
		orig := errors.New("commit: connection reset")
		got := WrapIfCommitInterrupted(expiredCtx, orig)
		if !errors.Is(got, ErrCommitInterrupted) {
			t.Fatalf("expected ErrCommitInterrupted in the chain, got %v", got)
		}
		if !errors.Is(got, orig) {
			t.Fatalf("expected the original error still reachable via errors.Is, got %v", got)
		}
	})

	t.Run("nil err — no-op regardless of commitCtx state", func(t *testing.T) {
		if got := WrapIfCommitInterrupted(expiredCtx, nil); got != nil {
			t.Fatalf("nil err must pass through as nil, got %v", got)
		}
	})
}

// errConflictStandIn stands in for spi.ErrConflict without importing the
// spi package into this file — WrapIfCommitInterrupted is agnostic to what
// the wrapped error is, only to commitCtx's state, so any sentinel proves
// the point equally well.
var errConflictStandIn = errors.New("transaction conflict (stand-in)")

// TestShieldedCommitWithBudget_RealExpiry_WrapsErrCommitInterrupted proves
// the wrap mechanism ShieldedCommit/ShieldedCommitWithBudget provides for
// REAL — not simulated — by injecting a short budget and a commit function
// that blocks until its ctx is genuinely Done (mirroring the deterministic
// blocking-store technique used on the entity-handler side; see
// internal/domain/entity/handler_reqtimeout_test.go's blockingEntityStore).
// The production commitBudget is a fixed 30s, impractical to wait out in a
// unit test — the injectable budget is what makes this provable fast.
func TestShieldedCommitWithBudget_RealExpiry_WrapsErrCommitInterrupted(t *testing.T) {
	origErr := errors.New("commit: connection reset mid-flight")
	var sawCommitCtx context.Context
	err := ShieldedCommitWithBudget(context.Background(), 5*time.Millisecond, func(commitCtx context.Context) error {
		sawCommitCtx = commitCtx
		<-commitCtx.Done() // the budget genuinely expires here — no simulation
		return origErr
	})

	if sawCommitCtx.Err() == nil {
		t.Fatal("test setup bug: commit ctx must show an error by the time this assertion runs")
	}
	if !errors.Is(err, ErrCommitInterrupted) {
		t.Fatalf("want ErrCommitInterrupted in the chain, got %v", err)
	}
	if !errors.Is(err, origErr) {
		t.Fatalf("want the original commit error still reachable via errors.Is, got %v", err)
	}
}

// TestShieldedCommit_CleanFailure_LiveBudget_Unwrapped is the negative half:
// a commit that fails fast, well within budget, must NOT be wrapped —
// ShieldedCommit only marks the case where the commit's OWN ctx was what
// failed it.
func TestShieldedCommit_CleanFailure_LiveBudget_Unwrapped(t *testing.T) {
	origErr := errConflictStandIn
	err := ShieldedCommit(context.Background(), func(context.Context) error {
		return origErr
	})
	if errors.Is(err, ErrCommitInterrupted) {
		t.Fatalf("a fast clean failure on a live budget must not be wrapped: %v", err)
	}
	if !errors.Is(err, origErr) {
		t.Fatalf("original error must still be reachable via errors.Is, got %v", err)
	}
}

// TestShieldedCommit_Success_ReturnsNil pins the happy path: a successful
// commit function's nil is returned unchanged.
func TestShieldedCommit_Success_ReturnsNil(t *testing.T) {
	if err := ShieldedCommit(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

// TestShieldedCommit_ClearsRequestTimeoutMarkerAndDropsCancellation pins that
// ShieldedCommit derives its ctx the same way CommitContext does: the marker
// is cleared and the caller's cancellation cannot reach the commit function,
// even when the caller's ctx is already cancelled before the call.
func TestShieldedCommit_ClearsRequestTimeoutMarkerAndDropsCancellation(t *testing.T) {
	parent, cancel := WithRequestTimeout(context.Background(), 60000)
	cancel() // cancel immediately — the shield must not observe this

	var observedMarker bool
	var observedErr error
	_ = ShieldedCommit(parent, func(commitCtx context.Context) error {
		observedMarker = HasRequestTimeout(commitCtx)
		observedErr = commitCtx.Err()
		return nil
	})
	if observedMarker {
		t.Fatal("commit ctx must not carry the client-requested-timeout marker")
	}
	if observedErr != nil {
		t.Fatalf("commit ctx must not observe the caller's cancellation, got %v", observedErr)
	}
}

func TestCommitContext_DetachedAndBudgeted(t *testing.T) {
	parent, cancel := WithRequestTimeout(context.WithValue(context.Background(), ucKey{}, "tenant-a"), 1)
	defer cancel()
	<-parent.Done()
	cctx, ccancel := CommitContext(parent)
	defer ccancel()
	if cctx.Err() != nil {
		t.Fatal("commit ctx must survive an expired parent")
	}
	if got := cctx.Value(ucKey{}); got != "tenant-a" {
		t.Fatalf("commit context lost parent values: got %v", got)
	}
	dl, ok := cctx.Deadline()
	if !ok {
		t.Fatal("commit ctx must carry its own budget")
	}
	if d := time.Until(dl); d <= 0 || d > 30*time.Second {
		t.Fatalf("deadline %v is not the documented 30s bound", d)
	}
}

func TestCommitContext_ClearsRequestTimeoutMarker(t *testing.T) {
	parent, cancel := WithRequestTimeout(context.Background(), 60000)
	defer cancel()
	if !HasRequestTimeout(parent) {
		t.Fatal("parent must carry the marker")
	}
	cctx, ccancel := CommitContext(parent)
	defer ccancel()
	if HasRequestTimeout(cctx) {
		t.Fatal("commit ctx must not carry the client-requested-timeout marker — a commitBudget expiry is not the client's 408")
	}
	if !HasRequestTimeout(parent) {
		t.Fatal("clearing the marker on the derived ctx must not mutate the parent")
	}
	err := fmt.Errorf("failed to commit transaction: %w", context.DeadlineExceeded)
	if got := ClassifyRequestTimeout(cctx, err, ErrCodeTransactionTimeout); got != nil {
		t.Fatalf("commit ctx must never classify as the client's 408, got %v", got)
	}
}
