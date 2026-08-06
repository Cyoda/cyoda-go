package grpc

// errors_test.go — what buildErrorFields puts in the LOG, as distinct from what
// it puts on the wire.

import (
	"errors"
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
