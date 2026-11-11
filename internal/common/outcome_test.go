package common

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

// WriteRecordedFailure sends the recorded failure as WriteError answered it,
// under the same ticket, and logs nothing: it was logged when it was answered.
// A later failure that was not recorded does not displace it.
func TestWriteRecordedFailure(t *testing.T) {
	if WriteRecordedFailure(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil)) {
		t.Fatal("wrote an answer with no holder")
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx, _ := WithOutcome(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/x", nil).WithContext(ctx)
	first := httptest.NewRecorder()
	WriteError(first, r, Internal("first", errors.New("boom")))
	WriteError(httptest.NewRecorder(), r, Internal("second", errors.New("bang")))
	logged := logs.Len()

	again := httptest.NewRecorder()
	if !WriteRecordedFailure(ctx, again, r) {
		t.Fatal("no recorded failure was written")
	}
	if again.Code != first.Code || again.Body.String() != first.Body.String() {
		t.Fatalf("resent %d %s; want the first answer %d %s", again.Code, again.Body.String(), first.Code, first.Body.String())
	}
	if logs.Len() != logged {
		t.Fatalf("resending logged again: %s", logs.String()[logged:])
	}
}
