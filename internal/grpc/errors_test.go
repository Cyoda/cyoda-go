package grpc

// errors_test.go — what buildErrorFields puts in the LOG, as distinct from what
// it puts on the wire.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestBuildErrorFields_OperationalCauseIsLogged — an operational AppError may
// carry a cause attached with WithCause: infrastructure detail that must stay
// out of the envelope but is the operator's ONLY breadcrumb for why (say) the
// connection pool could not serve a write. The HTTP door logs it
// (common.WriteError's operational branch); this door must too, under the same
// message and field name, or the same failure is undiagnosable depending on
// which entry point the client happened to use.
func TestBuildErrorFields_OperationalCauseIsLogged(t *testing.T) {
	records := captureSlog(t)

	cause := errors.New("Begin: could not acquire a database connection within the configured timeout")
	appErr := common.Operational(
		http.StatusServiceUnavailable,
		common.ErrCodeStorageUnavailable,
		"storage is temporarily unavailable — retry",
	).AsRetryable().WithCause(cause)

	code, message, retryable := buildErrorFields(context.Background(), appErr)

	if code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR (the envelope class)", code)
	}
	if retryable == nil || !*retryable {
		t.Errorf("retryable = %v, want true", retryable)
	}
	// Gate 3: the cause is for the log, never the wire.
	if strings.Contains(message, cause.Error()) {
		t.Errorf("envelope message leaks the infrastructure cause: %q", message)
	}

	rec := findRecord(t, records, "operational error")
	if got, _ := rec.attrs["cause"].(string); got != cause.Error() {
		t.Errorf("logged cause = %q, want %q", got, cause.Error())
	}
	if got, _ := rec.attrs["code"].(string); got != common.ErrCodeStorageUnavailable {
		t.Errorf("logged code = %q, want %q", got, common.ErrCodeStorageUnavailable)
	}
}

// TestBuildErrorFields_OperationalWithoutCauseLogsNothing — the breadcrumb is
// for errors that carry one. An ordinary 4xx puts its full detail in the
// message the client already receives, so logging it again would be noise on
// every bad request.
func TestBuildErrorFields_OperationalWithoutCauseLogsNothing(t *testing.T) {
	records := captureSlog(t)

	appErr := common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound, "model not found")
	if code, _, _ := buildErrorFields(context.Background(), appErr); code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", code)
	}

	for _, r := range *records {
		t.Errorf("a causeless operational error logged %q at %v", r.msg, r.level)
	}
}

// TestBuildErrorFields_HonoursAPinnedTicket — an AppError may arrive with its
// ticket already pinned, because the caller has already logged the full detail
// under it (the panic recovery middlewares do exactly this). Minting a second
// one here would hand the client an identifier that names no log line, and would
// leave the pinned field silently meaning nothing on this door while meaning
// something on the HTTP one.
func TestBuildErrorFields_HonoursAPinnedTicket(t *testing.T) {
	const pinned = "11111111-2222-4333-8444-555555555555"
	appErr := common.Internal("something broke", errors.New("detail for the log")).WithTicket(pinned)

	code, message, _ := buildErrorFields(context.Background(), appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, pinned) {
		t.Errorf("message = %q, want the pinned ticket %s — a fresh one names no log line", message, pinned)
	}
}

// cancelledContext returns a context.Context that is already Done with
// context.Canceled — the serving RPC's own context after its client has gone
// away.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestBuildErrorFields_ClientGoneCancellation_LogsDebugNoTicket — a client
// that goes away mid-request is not a server fault. Nothing was wrong and
// there is nobody to quote a ticket to; this is exactly the moment (a
// compute member failing over) an operator wants a clean log. Both conjuncts
// of the recognition hold here: the cause carries context.Canceled AND the
// serving context itself is done.
func TestBuildErrorFields_ClientGoneCancellation_LogsDebugNoTicket(t *testing.T) {
	records := captureSlog(t)

	appErr := common.Internal("dispatch request ended before it took the transaction's lock", context.Canceled)
	code, message, _ := buildErrorFields(cancelledContext(), appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if strings.Contains(message, "ticket") {
		t.Errorf("envelope message must not carry a ticket nobody can be quoted: %q", message)
	}
	for _, r := range *records {
		if r.level == slog.LevelError {
			t.Errorf("a client disconnect must not log at ERROR: %+v", r)
		}
		if _, ok := r.attrs["ticket"]; ok {
			t.Errorf("a client disconnect must not mint or log a ticket: %+v", r)
		}
	}
	findRecord(t, records, "client gone before request completed")
}

// TestBuildErrorFields_ClientGoneCancellation_RawError — the same recognition
// on the unclassified-raw-error branch (which the comment there calls
// "should not happen", but the client having gone can still surface as a bare
// context.Canceled that never passed through an *AppError).
func TestBuildErrorFields_ClientGoneCancellation_RawError(t *testing.T) {
	records := captureSlog(t)

	code, message, _ := buildErrorFields(cancelledContext(), context.Canceled)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if strings.Contains(message, "ticket") {
		t.Errorf("envelope message must not carry a ticket nobody can be quoted: %q", message)
	}
	for _, r := range *records {
		if r.level == slog.LevelError {
			t.Errorf("a client disconnect must not log at ERROR: %+v", r)
		}
	}
}

// TestBuildErrorFields_JoinedHandlerContextNeverDone_KeepsTicket is the case
// the fresh-context review caught: internal/domain/txjoin/txjoin.go:212 does
// `joined = context.WithoutCancel(joined)` once a joined request has the
// transaction, precisely so a compute member going away mid-statement cannot
// take the connection with it. From there classifyWorkflowError
// (internal/domain/entity/service.go:2853-2855) still mints
// common.Internal("workflow aborted by context cancellation", err) from any
// context.Canceled it sees — by its own comment, that case must stay a
// ticketed 500, because the cancellation may be unrelated to this (detached,
// still-live) request. Recognising client-gone on the error chain alone would
// wrongly quieten a genuine failure on a client that is still connected and
// waiting for its answer. The two-conjunct test — chain carries
// context.Canceled AND the passed-in ctx is itself done — must not fire here,
// because ctx.Err() is nil on a WithoutCancel-derived context no matter what
// its parent is doing.
func TestBuildErrorFields_JoinedHandlerContextNeverDone_KeepsTicket(t *testing.T) {
	records := captureSlog(t)

	parent, cancel := context.WithCancel(context.Background())
	cancel() // the client that queued for the lock has gone
	joined := context.WithoutCancel(parent) // ...but the joined ctx detaches, as txjoin.go does

	appErr := common.Internal("workflow aborted by context cancellation", context.Canceled)
	code, message, _ := buildErrorFields(joined, appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, "ticket") {
		t.Errorf("message = %q, want a ticket — a WithoutCancel-derived context is never done, so this is not a client disconnect", message)
	}
	rec := findRecord(t, records, "internal error")
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want ERROR", rec.level)
	}
	if _, ok := rec.attrs["ticket"]; !ok {
		t.Error("expected a ticket attribute on the log record")
	}
}

// TestBuildErrorFields_RawError_ContextNotDone_KeepsTicket mirrors the joined
// case above on the raw-error branch: a bare context.Canceled whose passed-in
// ctx is not done must not be quietened either.
func TestBuildErrorFields_RawError_ContextNotDone_KeepsTicket(t *testing.T) {
	records := captureSlog(t)

	code, message, _ := buildErrorFields(context.Background(), context.Canceled)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, "ticket") {
		t.Errorf("message = %q, want a ticket — the passed-in context is not done", message)
	}
	rec := findRecord(t, records, "unclassified error")
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want ERROR", rec.level)
	}
}

// TestBuildErrorFields_OrdinaryInternalError_StillLogsErrorWithTicket pins
// the regression this task must not cause: an ordinary internal error still
// mints a ticket and logs at ERROR.
func TestBuildErrorFields_OrdinaryInternalError_StillLogsErrorWithTicket(t *testing.T) {
	records := captureSlog(t)

	appErr := common.Internal("something broke", errors.New("db connection failed"))
	code, message, _ := buildErrorFields(context.Background(), appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, "ticket") {
		t.Errorf("message = %q, want a ticket", message)
	}
	rec := findRecord(t, records, "internal error")
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want ERROR", rec.level)
	}
	if _, ok := rec.attrs["ticket"]; !ok {
		t.Error("expected a ticket attribute on the log record")
	}
}

// TestBuildErrorFields_FeatureDeadlineTimeout_Untouched: a cause carrying
// context.DeadlineExceeded (a feature-deadline timeout, already classified to
// 408 upstream by ClassifyRequestTimeout before it would ever reach here) must
// not be treated as a client disconnect, even when the serving context is
// itself done via that same deadline.
func TestBuildErrorFields_FeatureDeadlineTimeout_Untouched(t *testing.T) {
	records := captureSlog(t)

	ctx, cancel := context.WithTimeout(context.Background(), 0) // already expired
	defer cancel()
	<-ctx.Done()

	appErr := common.Internal("workflow aborted by context cancellation", context.DeadlineExceeded)
	code, message, _ := buildErrorFields(ctx, appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, "ticket") {
		t.Errorf("message = %q, want a ticket — a DeadlineExceeded cause is not a client disconnect", message)
	}
	rec := findRecord(t, records, "internal error")
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want ERROR", rec.level)
	}
}
