package grpc

// errors_test.go — what buildErrorFields puts in the LOG, as distinct from what
// it puts on the wire.

import (
	"context"
	"errors"
	"fmt"
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

	code, message, retryable := buildErrorFields(appErr)

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
	if code, _, _ := buildErrorFields(appErr); code != "CLIENT_ERROR" {
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

	code, message, _ := buildErrorFields(appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, pinned) {
		t.Errorf("message = %q, want the pinned ticket %s — a fresh one names no log line", message, pinned)
	}
}

// clientGoneCause is the join layer's own "the request ended before it took the
// transaction's lock", as internal/domain/txjoin marks it. It is the one cause
// a door's funnel may file as a departed client.
func clientGoneCause() error {
	return fmt.Errorf("%w: %w", common.ErrClientGone, context.Canceled)
}

// TestBuildErrorFields_ClientGoneCancellation_LogsDebugNoTicket — a client
// that goes away mid-request is not a server fault. Nothing was wrong and
// there is nobody to quote a ticket to; this is exactly the moment (a
// compute member failing over) an operator wants a clean log.
func TestBuildErrorFields_ClientGoneCancellation_LogsDebugNoTicket(t *testing.T) {
	records := captureSlog(t)

	appErr := common.Internal("dispatch request ended before it took the transaction's lock", clientGoneCause())
	code, message, _ := buildErrorFields(appErr)

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
	rec := findRecord(t, records, "client gone before request completed")
	// The HTTP funnel logs this same event; the two must collate, so they carry
	// the same fields (the HTTP door adds the request path, which this door has
	// no equivalent of).
	for _, field := range []string{"code", "message", "detail"} {
		if _, ok := rec.attrs[field]; !ok {
			t.Errorf("the client-gone line is missing %q, so it does not collate with the HTTP funnel's: %+v", field, rec)
		}
	}
}

// TestBuildErrorFields_ClientGoneCancellation_RawError — the same recognition
// on the unclassified-raw-error branch (which the comment there calls
// "should not happen", but a departed client can still reach this funnel as a
// marked cause that never passed through an *AppError).
func TestBuildErrorFields_ClientGoneCancellation_RawError(t *testing.T) {
	records := captureSlog(t)

	code, message, _ := buildErrorFields(clientGoneCause())

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

// TestBuildErrorFields_InternalFailureWrappingACancellation_KeepsItsTicket is
// the regression that matters: a joined request is deliberately detached from
// its client (txjoin uses context.WithoutCancel), so work that outlives its
// caller can fail with an internal error that merely wraps an unrelated
// context.Canceled — classifyWorkflowError wraps any such cause as Internal.
// Filing that as a departed client would log a genuine fault at DEBUG with no
// ticket, below the default level: the fault would leave no trace at all.
func TestBuildErrorFields_InternalFailureWrappingACancellation_KeepsItsTicket(t *testing.T) {
	records := captureSlog(t)

	appErr := common.Internal("workflow aborted by context cancellation",
		fmt.Errorf("failed to join transaction: %w", context.Canceled))
	code, message, _ := buildErrorFields(appErr)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, "ticket") {
		t.Errorf("message = %q, want a ticket — an internal failure that merely wraps a cancellation is a fault", message)
	}
	rec := findRecord(t, records, "internal error")
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want ERROR", rec.level)
	}
	if _, ok := rec.attrs["ticket"]; !ok {
		t.Error("expected a ticket attribute on the log record")
	}
}

// TestBuildErrorFields_RawCancellation_KeepsTicket — a bare context.Canceled
// that reaches this funnel unmarked is not a departed client: nothing said it
// was. It keeps its ticket, and the operator has a trace of it.
func TestBuildErrorFields_RawCancellation_KeepsTicket(t *testing.T) {
	records := captureSlog(t)

	code, message, _ := buildErrorFields(context.Canceled)

	if code != "SERVER_ERROR" {
		t.Errorf("code = %q, want SERVER_ERROR", code)
	}
	if !strings.Contains(message, "ticket") {
		t.Errorf("message = %q, want a ticket — an unmarked cancellation is not a departed client", message)
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
	code, message, _ := buildErrorFields(appErr)

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
// not be treated as a client disconnect.
func TestBuildErrorFields_FeatureDeadlineTimeout_Untouched(t *testing.T) {
	records := captureSlog(t)

	appErr := common.Internal("workflow aborted by context cancellation", context.DeadlineExceeded)
	code, message, _ := buildErrorFields(appErr)

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
