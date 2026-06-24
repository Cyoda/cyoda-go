package observability

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestInstrErr_LogsOnError(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	instrErr("cyoda.test.metric", errors.New("boom"))
	out := buf.String()
	if !strings.Contains(out, "cyoda.test.metric") || !strings.Contains(out, "boom") {
		t.Fatalf("expected instrument+error logged, got %q", out)
	}
}

func TestInstrErr_NoLogOnNil(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	instrErr("cyoda.test.metric", nil)
	if buf.Len() != 0 {
		t.Fatalf("expected no log on nil error, got %q", buf.String())
	}
}
