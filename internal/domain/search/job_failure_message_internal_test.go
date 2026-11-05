package search

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

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

// A raw, unclassified failure is classified as the doors classify it: a
// server-side failure, recorded with a ticket and logged once, at ERROR, with
// the job id and the detail.
func TestJobFailureMessage_RawErrorIsTicketed(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := jobFailureMessage("job-9", errors.New("driver: connection reset"))
	if !strings.HasPrefix(got, "SERVER_ERROR: internal error [ticket: ") {
		t.Fatalf("reason = %q, want the ticketed generic text", got)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "job-9") || !strings.Contains(out, "driver: connection reset") {
		t.Fatalf("want one ERROR line with the job id and detail:\n%s", out)
	}
}

// A job released for reclaim (or superseded) records no failure, so its
// reason is never rendered: no ticket is minted, and nothing is logged, for a
// record that is not written.
func TestWriteAsyncFailure_ReleasedJobRendersNoReason(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errJobReleased)
	rendered := false
	(&SearchService{}).writeAsyncFailure(ctx, "job-r", 1, func() string { rendered = true; return "x" }, time.Now(), 0)
	if rendered {
		t.Fatal("the reason of a released job was rendered")
	}
}
