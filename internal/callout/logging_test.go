package callout

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer lets cnode writer goroutines log while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ownerInfoLines captures the INFO lines of pkg=callout written while run runs.
func ownerInfoLines(t *testing.T, run func()) []map[string]any {
	t.Helper()
	out := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	run()

	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		if line["pkg"] == "callout" && line["level"] == "INFO" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestOwner_OneInfoLine_ForACalloutThatNeededMoreThanOneTry(t *testing.T) {
	yes := true
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", fails("card 4111-1111 declined", &yes))

	lines := ownerInfoLines(t, func() { _, _ = e.dispatchFunction(userCtx(tenantA), "x", "") })

	if len(lines) != 1 {
		t.Fatalf("INFO lines = %v, want exactly one", lines)
	}
	line := lines[0]
	if line["tries"] != float64(2) || line["tenantId"] != string(tenantA) || line["tags"] != "x" || line["succeeded"] != false {
		t.Errorf("line = %v, want tries=2, the tenant, the tags and succeeded=false", line)
	}
	if _, ok := line["elapsedMs"]; !ok {
		t.Errorf("line = %v, want elapsedMs", line)
	}
	for key, value := range line {
		if s, ok := value.(string); ok && strings.Contains(s, "4111") {
			t.Errorf("field %s repeats the cnode's own failure text, which is tenant content: %q", key, s)
		}
	}
}

func TestOwner_OneInfoLine_ForACalloutThatWaited(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second})
	time.AfterFunc(50*time.Millisecond, func() { e.attach(t, "m-1", tenantA, "x", answers("m-1")) })

	lines := ownerInfoLines(t, func() {
		if _, err := e.dispatchFunction(userCtx(tenantA), "x", ""); err != nil {
			t.Errorf("dispatch: %v", err)
		}
	})

	if len(lines) != 1 || lines[0]["tries"] != float64(1) || lines[0]["succeeded"] != true {
		t.Fatalf("INFO lines = %v, want one line: one try, succeeded, after a wait", lines)
	}
	if waited, _ := lines[0]["waitedMs"].(float64); waited <= 0 {
		t.Errorf("waitedMs = %v, want the wait on record", lines[0]["waitedMs"])
	}
}

func TestOwner_NoInfoLine_ForACalloutAnsweredByItsFirstTry(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	lines := ownerInfoLines(t, func() { _, _ = e.dispatchFunction(userCtx(tenantA), "x", "") })

	if len(lines) != 0 {
		t.Errorf("INFO lines = %v, want none: the ordinary callout is logged at DEBUG only", lines)
	}
}
