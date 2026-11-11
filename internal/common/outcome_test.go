package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecordOutcome_FirstFailureWins(t *testing.T) {
	ctx, o := WithOutcome(context.Background())
	RecordOutcome(ctx, Operational(http.StatusBadRequest, ErrCodeBadRequest, "first"))
	RecordOutcome(ctx, Operational(http.StatusConflict, ErrCodeConflict, "second").AsRetryable())
	if o.Status != http.StatusBadRequest || o.Code != ErrCodeBadRequest || o.Retryable {
		t.Errorf("recorded %d %s retryable=%v, want the first failure", o.Status, o.Code, o.Retryable)
	}
}

func TestRecordOutcome_NoHolderIsNoOp(t *testing.T) {
	RecordOutcome(context.Background(), Operational(http.StatusBadRequest, ErrCodeBadRequest, "x"))
}

func TestWriteError_RecordsOutcome(t *testing.T) {
	cause := errors.New("lost the race")
	ctx, o := WithOutcome(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/x", nil).WithContext(ctx)
	WriteError(httptest.NewRecorder(), r,
		Operational(http.StatusConflict, ErrCodeConflict, "conflict").AsRetryable().WithCause(cause))
	if o.Status != http.StatusConflict || o.Code != ErrCodeConflict || !o.Retryable {
		t.Errorf("recorded %d %s retryable=%v", o.Status, o.Code, o.Retryable)
	}
	if !errors.Is(o.Err, cause) {
		t.Errorf("o.Err = %v, want the cause chain intact", o.Err)
	}
}

func TestRecordedFailure(t *testing.T) {
	if RecordedFailure(context.Background()) != nil {
		t.Fatal("a context with no holder reads as failed")
	}
	ctx, _ := WithOutcome(context.Background())
	if RecordedFailure(ctx) != nil {
		t.Fatal("a fresh holder reads as failed")
	}
	recorded := Operational(http.StatusNotFound, ErrCodeEntityNotFound, "gone")
	RecordOutcome(ctx, recorded)
	if got := RecordedFailure(ctx); got != recorded {
		t.Fatalf("RecordedFailure = %v; want the recorded error", got)
	}
}
