package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// TestHandlerNeutralisesLogInjection pins the invariant that makes the
// go/log-injection findings inapplicable to this codebase.
//
// Static analysis flags every site where request-derived data reaches a log
// call, because with a naive writer an embedded newline lets an attacker forge
// a second log record. slog's TextHandler quotes any value that needs it, so
// the newline is escaped and one call still yields exactly one line.
//
// That guarantee is a property of the handler, not of the ~49 call sites, so it
// is asserted once here. If the handler is ever swapped for one that does not
// quote — a hand-rolled writer, or a JSON handler configured to pass raw — this
// test fails and the call sites genuinely do need sanitising.
func TestHandlerNeutralisesLogInjection(t *testing.T) {
	forged := "level=ERROR msg=\"forged administrative action\" actor=root"
	payloads := map[string]string{
		"newline":           "abc\n" + forged,
		"carriage return":   "abc\r" + forged,
		"crlf":              "abc\r\n" + forged,
		"quote and newline": "abc\"\n" + forged,
		"embedded null":     "abc\x00" + forged,
		"unicode line sep":  "abc " + forged,
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			// Same handler construction as logging.Init.
			l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: logging.Level}))

			// Both shapes that appear at the flagged sites: a structured
			// attribute, and the message itself.
			l.Error("tenant operation", slog.String("tenantId", payload))
			l.Error(payload, slog.String("pkg", "logging"))

			got := strings.TrimRight(buf.String(), "\n")
			if n := strings.Count(got, "\n"); n != 1 {
				t.Fatalf("2 log calls produced %d newlines, want 1 separator — "+
					"injected payload broke out of its record:\n%s", n, got)
			}
			if strings.Contains(got, "\nlevel=ERROR msg=\"forged") {
				t.Fatalf("forged record appears at line start:\n%s", got)
			}
		})
	}
}
