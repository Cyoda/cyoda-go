package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// captureLog redirects slog default for the duration of fn and returns the
// recorded JSON-line records.
func captureLog(t *testing.T, level slog.Level, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	fn()

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("failed to parse log line %q: %v", line, err)
		}
		records = append(records, m)
	}
	return records
}

func TestLogCORSMode_Disabled(t *testing.T) {
	cfg := app.CORSConfig{Enabled: false}
	records := captureLog(t, slog.LevelDebug, func() { logCORSMode(cfg) })
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(records), records)
	}
	r := records[0]
	if r["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", r["level"])
	}
	if !strings.Contains(r["msg"].(string), "cors: disabled") {
		t.Errorf("msg = %q, want substring \"cors: disabled\"", r["msg"])
	}
	if r["pkg"] != "cors" {
		t.Errorf("pkg = %v, want \"cors\"", r["pkg"])
	}
}

func TestLogCORSMode_Wildcard(t *testing.T) {
	cfg := app.CORSConfig{Enabled: true, Wildcard: true}
	records := captureLog(t, slog.LevelDebug, func() { logCORSMode(cfg) })
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(records), records)
	}
	r := records[0]
	if r["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", r["level"])
	}
	if !strings.Contains(r["msg"].(string), "wildcard mode active") {
		t.Errorf("msg = %q, want \"wildcard mode active\" substring", r["msg"])
	}
}

func TestLogCORSMode_Loopback(t *testing.T) {
	cfg := app.CORSConfig{Enabled: true}
	records := captureLog(t, slog.LevelDebug, func() { logCORSMode(cfg) })
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(records), records)
	}
	r := records[0]
	if r["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", r["level"])
	}
	if !strings.Contains(r["msg"].(string), "loopback mode active") {
		t.Errorf("msg = %q, want \"loopback mode active\" substring", r["msg"])
	}
}

func TestLogCORSMode_Allowlist_InfoOnly(t *testing.T) {
	cfg := app.CORSConfig{Enabled: true, AllowedOrigins: []string{"https://a.com", "https://b.com"}}
	records := captureLog(t, slog.LevelInfo, func() { logCORSMode(cfg) })
	if len(records) != 1 {
		t.Fatalf("at INFO level we should see exactly the count record, got %d records: %v", len(records), records)
	}
	r := records[0]
	if r["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", r["level"])
	}
	if !strings.Contains(r["msg"].(string), "allowlist mode active") {
		t.Errorf("msg = %q", r["msg"])
	}
	if cnt, ok := r["origin_count"].(float64); !ok || int(cnt) != 2 {
		t.Errorf("origin_count = %v, want 2", r["origin_count"])
	}
	// Spec: contents must NOT appear at INFO. Verify no "origins" key.
	if _, present := r["origins"]; present {
		t.Errorf("origins key leaked into INFO record: %v", r)
	}
}

func TestLogCORSMode_Allowlist_DebugIncludesOrigins(t *testing.T) {
	cfg := app.CORSConfig{Enabled: true, AllowedOrigins: []string{"https://a.com", "https://b.com"}}
	records := captureLog(t, slog.LevelDebug, func() { logCORSMode(cfg) })
	if len(records) != 2 {
		t.Fatalf("at DEBUG level we should see count + contents = 2 records, got %d: %v", len(records), records)
	}
	// First record: INFO with count.
	if records[0]["level"] != "INFO" {
		t.Errorf("first record level = %v, want INFO", records[0]["level"])
	}
	// Second record: DEBUG with origins slice.
	if records[1]["level"] != "DEBUG" {
		t.Errorf("second record level = %v, want DEBUG", records[1]["level"])
	}
	origins, ok := records[1]["origins"].([]any)
	if !ok {
		t.Fatalf("origins is not a slice: %v", records[1]["origins"])
	}
	if len(origins) != 2 {
		t.Errorf("origins has %d entries, want 2", len(origins))
	}
}

func TestLogCORSMode_UnknownMode(t *testing.T) {
	// Construct a config that produces a Mode() Go currently doesn't know
	// about. Easiest: simulate via a custom value — but Mode() is hard-coded
	// to four cases, so we synthesize by calling logCORSMode through the
	// default branch: this requires the default branch to exist. The test
	// asserts that even an unrecognised mode produces a visible WARN, not
	// silent omission.
	//
	// Since Mode() can't currently return anything else, we invoke
	// logCORSMode with the default case via reflection of the underlying
	// helper if exposed, or skip this test if the default branch is the
	// only way to reach it. For now, assert the default branch fires by
	// monkeying CORSConfig such that Mode() returns "" — Mode() returns
	// "disabled" when Enabled=false, "wildcard" when Wildcard, "loopback"
	// when no origins, "allowlist" otherwise. There is no input that
	// produces an unknown mode through Mode(). We test the default case
	// indirectly by checking that the logCORSMode source has a default
	// branch — but that's a compile-time assertion. We'll use a workaround:
	// the default branch is reached only if Mode() ever gains a new value;
	// we can verify it exists by reading the source. Skip the runtime test.
	t.Skip("Mode() currently can't produce an unknown value; default branch is defensive only")
}
