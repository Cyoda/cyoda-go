package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// A client error that carries its own log level (a boundary/backend
// disagreement at WARN) is logged once at that level on the async path too,
// as both doors' funnels log it.
func TestJobFailureMessage_ClientErrorKeepsItsLogLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	refusal := common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, "condition contains a pattern operand the backend cannot compile").
		WithCause(errors.New("pattern disagreement")).WithLogLevel(slog.LevelWarn)
	jobFailureMessage("job-9", refusal)
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("log = %q, want one WARN line", buf.String())
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
// reason is never rendered: no recorded reason is minted. A server-side fault
// behind it did happen, and is logged once, marked discarded; a client error
// behind it is not an event of its own.
func TestWriteAsyncFailure_ReleasedJobRendersNoReason(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errJobReleased)
	rendered := false
	failure := failureReason("job-r", errors.New("driver: connection reset"))
	render := failure.render
	failure.render = func() string { rendered = true; return render() }
	(&SearchService{}).writeAsyncFailure(ctx, &asyncJobHandle{cancel: cancel, ctx: ctx}, "job-r", 1, failure, time.Now(), 0)
	if rendered {
		t.Fatal("the reason of a released job was rendered")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=ERROR") || !strings.Contains(lines[0], "attempt=discarded") || !strings.Contains(lines[0], "connection reset") {
		t.Errorf("log = %q, want the fault logged once, marked discarded", lines)
	}

	buf.Reset()
	(&SearchService{}).writeAsyncFailure(ctx, &asyncJobHandle{cancel: cancel, ctx: ctx}, "job-r3", 1, failureReason("job-r3", fmt.Errorf("load model: %w", context.Canceled)), time.Now(), 0)
	if buf.Len() != 0 {
		t.Errorf("a step failed by the job's own cancellation was logged as a fault: %s", buf.String())
	}

	buf.Reset()
	client := common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, "condition or sort references an invalid field path")
	(&SearchService{}).writeAsyncFailure(ctx, &asyncJobHandle{cancel: cancel, ctx: ctx}, "job-r2", 1, failureReason("job-r2", client), time.Now(), 0)
	if buf.Len() != 0 {
		t.Errorf("a client error behind a released job was logged: %s", buf.String())
	}
}

// ceilingFault stands for a backend's async search ceiling breach.
type ceilingFault struct{}

func (ceilingFault) Error() string               { return "statement exceeded the async search ceiling" }
func (ceilingFault) SearchCeilingExceeded() bool { return true }

// A ceiling breach behind a job that ended unrecorded is the request's own
// outcome, as recorded (INFO), not a server-side fault: nothing is logged for
// it as discarded.
func TestLogUnrecordedJobFailure_CeilingIsNotAFault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logUnrecordedJobFailure("job-c", fmt.Errorf("scan: %w", ceilingFault{}))
	if buf.Len() != 0 {
		t.Errorf("a ceiling breach was logged as a fault: %s", buf.String())
	}
}
