package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestHoldsNUL(t *testing.T) {
	const nul = "a\x00b"
	tests := []struct {
		name    string
		state   string
		details string
		data    map[string]any
		want    bool
	}{
		{"none", "S", "d", map[string]any{"k": "v", "n": 1, "b": true}, false},
		{"nil data", "S", "d", nil, false},
		{"state", nul, "d", nil, true},
		{"details", "S", nul, nil, true},
		{"top-level value", "S", "d", map[string]any{"k": nul}, true},
		{"top-level key", "S", "d", map[string]any{nul: "v"}, true},
		{"nested key", "S", "d", map[string]any{"o": map[string]any{nul: "v"}}, true},
		{"nested value", "S", "d", map[string]any{"o": map[string]any{"k": nul}}, true},
		{"[]string", "S", "d", map[string]any{"processNames": []string{"ok", nul}}, true},
		{"[]string clean", "S", "d", map[string]any{"processNames": []string{"ok"}}, false},
		{"[]any of maps", "S", "d", map[string]any{"functions": []any{map[string]any{"name": "ok"}, map[string]any{"name": nul}}}, true},
		{"[]any of strings", "S", "d", map[string]any{"l": []any{"ok", nul}}, true},
		{"[]map[string]any", "S", "d", map[string]any{"l": []map[string]any{{"k": "ok"}, {"k": nul}}}, true},
		{"[]map[string]any key", "S", "d", map[string]any{"l": []map[string]any{{nul: "ok"}}}, true},
		{"[]any clean", "S", "d", map[string]any{"l": []any{map[string]any{"k": "v"}, int64(3)}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdsNUL(tc.state, tc.details, tc.data); got != tc.want {
				t.Errorf("holdsNUL = %t, want %t", got, tc.want)
			}
		})
	}
}

type countingAuditStore struct {
	spi.StateMachineAuditStore
	recorded []spi.StateMachineEvent
}

func (s *countingAuditStore) Record(_ context.Context, _ string, ev spi.StateMachineEvent) error {
	s.recorded = append(s.recorded, ev)
	return nil
}

func TestRecordEvent_RefusesNUL(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const evType = spi.StateMachineEventType("TRANSITION_NOT_MATCH_CRITERION")
	e := &Engine{}
	store := &countingAuditStore{}
	e.recordEvent(store, context.Background(), "ent-1", "tx-1", "S",
		evType, "details", map[string]any{"reason": "secret\x00text"})

	if len(store.recorded) != 0 {
		t.Fatalf("an event holding U+0000 was recorded: %+v", store.recorded)
	}
	out := buf.String()
	for _, want := range []string{"ent-1", string(evType), `"level":"WARN"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not hold %q", out, want)
		}
	}
	if strings.Contains(out, "secret") {
		t.Errorf("log holds the refused text: %q", out)
	}

	e.recordEvent(store, context.Background(), "ent-1", "tx-1", "S",
		evType, "details", map[string]any{"reason": "fine"})
	if len(store.recorded) != 1 {
		t.Errorf("a clean event was not recorded: %d", len(store.recorded))
	}
}
