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

func TestCommitContext_DetachedAndBudgeted(t *testing.T) {
	parent, cancel := WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-parent.Done()
	cctx, ccancel := CommitContext(parent)
	defer ccancel()
	if cctx.Err() != nil {
		t.Fatal("commit ctx must survive an expired parent")
	}
	if _, ok := cctx.Deadline(); !ok {
		t.Fatal("commit ctx must carry its own budget")
	}
}
