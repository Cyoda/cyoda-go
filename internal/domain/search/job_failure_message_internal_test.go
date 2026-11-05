package search

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// A server-side failure is recorded on the job as both doors answer it: the
// generic text and a ticket, with the detail in the log under that ticket. A
// client error keeps its own message, code first.
func TestJobFailureMessage_ServerSideFailureIsTicketed(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	msg := jobFailureMessage("job-7", common.Internal("failed to load model schema for condition validation", errors.New("model store: boom")))
	m := regexp.MustCompile(`^SERVER_ERROR: internal error \[ticket: ([0-9a-f-]{36})\]$`).FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("reason = %q, want the generic text with a ticket", msg)
	}
	if !strings.Contains(buf.String(), m[1]) || !strings.Contains(buf.String(), "model store: boom") || !strings.Contains(buf.String(), "job-7") {
		t.Fatalf("the detail is not logged under the ticket and the job id:\n%s", buf.String())
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("logged %d lines for one failure, want 1:\n%s", n, buf.String())
	}

	client := common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, "INVALID_FIELD_PATH: condition or sort references an invalid field path")
	if got := jobFailureMessage("job-8", client); got != client.Error() {
		t.Fatalf("client error reason = %q, want %q", got, client.Error())
	}
}

// An unclassified failure records the fixed fallback and logs its detail once,
// at ERROR, with the job id.
func TestJobFailureMessage_UnclassifiedFailureIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if got := jobFailureMessage("job-9", errors.New("driver: connection reset")); got != jobFailureFallback {
		t.Fatalf("reason = %q, want %q", got, jobFailureFallback)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "job-9") || !strings.Contains(out, "driver: connection reset") {
		t.Fatalf("want one ERROR line with the job id and detail:\n%s", out)
	}
}
