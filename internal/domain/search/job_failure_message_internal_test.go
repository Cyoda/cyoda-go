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

	msg := jobFailureMessage(common.Internal("failed to load model schema for condition validation", errors.New("model store: boom")))
	m := regexp.MustCompile(`^SERVER_ERROR: internal error \[ticket: ([0-9a-f-]{36})\]$`).FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("reason = %q, want the generic text with a ticket", msg)
	}
	if !strings.Contains(buf.String(), m[1]) || !strings.Contains(buf.String(), "model store: boom") {
		t.Fatalf("the detail is not logged under the ticket:\n%s", buf.String())
	}

	client := common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, "INVALID_FIELD_PATH: condition or sort references an invalid field path")
	if got := jobFailureMessage(client); got != client.Error() {
		t.Fatalf("client error reason = %q, want %q", got, client.Error())
	}
}
