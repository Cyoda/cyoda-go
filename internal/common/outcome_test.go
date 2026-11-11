package common

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestLogAnswer_HeldUntilTheScopeEnds: on a joined request (a holder on ctx),
// the recorded failure's line is held under the ticket the answer carries, and
// written once by WriteHeldLine. Without a holder, or for a failure that is not
// the recorded one, the line is written at once.
func TestLogAnswer_HeldUntilTheScopeEnds(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if ticket := LogAnswer(context.Background(), Internal("unjoined", errors.New("boom"))); ticket == "" || !strings.Contains(logs.String(), ticket) {
		t.Fatalf("no holder: ticket %q not logged at once: %s", ticket, logs.String())
	}
	logs.Reset()

	ctx, _ := WithOutcome(context.Background())
	recorded := Internal("joined", errors.New("boom"))
	RecordOutcome(ctx, recorded)
	ticket := LogAnswer(ctx, recorded, "path", "/x")
	if ticket == "" || logs.Len() != 0 {
		t.Fatalf("ticket %q, logged %q; want a ticket and nothing logged yet", ticket, logs.String())
	}
	if other := LogAnswer(ctx, Internal("not the recorded one", errors.New("bang"))); !strings.Contains(logs.String(), other) {
		t.Fatalf("a failure other than the recorded one was not logged at once: %s", logs.String())
	}
	logs.Reset()
	WriteHeldLine(ctx)
	WriteHeldLine(ctx)
	if n := strings.Count(logs.String(), `"msg":"internal error"`); n != 1 || !strings.Contains(logs.String(), `"ticket":"`+ticket+`"`) {
		t.Fatalf("held line written %d times, or not under ticket %s: %s", n, ticket, logs.String())
	}
}

// TestFoldHeldLine: the held line is not written; the answer that replaces the
// handler's carries it in its Detail and is logged at least at its level.
func TestFoldHeldLine(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, _ := WithOutcome(context.Background())
	recorded := Internal("store read failed", errors.New("session gone"))
	RecordOutcome(ctx, recorded)
	LogAnswer(ctx, recorded)
	answer := Operational(http.StatusNotFound, ErrCodeTransactionNotFound, "transaction not found")
	FoldHeldLine(ctx, answer)
	WriteHeldLine(ctx)
	if logs.Len() != 0 {
		t.Fatalf("the folded line was written: %s", logs.String())
	}
	if !strings.Contains(answer.Detail, "store read failed") || !strings.Contains(answer.Detail, "session gone") {
		t.Fatalf("Detail = %q; want the handler's failure", answer.Detail)
	}
	if answer.OperationalLogLevel() != slog.LevelError {
		t.Fatalf("level = %v; want ERROR, the handler's", answer.OperationalLogLevel())
	}
	quiet := Operational(http.StatusBadRequest, ErrCodeBadRequest, "x").WithLogLevel(slog.LevelWarn)
	ctx2, _ := WithOutcome(context.Background())
	r2 := Operational(http.StatusConflict, ErrCodeConflict, "y")
	RecordOutcome(ctx2, r2)
	LogAnswer(ctx2, r2)
	FoldHeldLine(ctx2, quiet)
	if quiet.OperationalLogLevel() != slog.LevelWarn {
		t.Fatalf("level = %v; a folded INFO line must not lower WARN", quiet.OperationalLogLevel())
	}
}
