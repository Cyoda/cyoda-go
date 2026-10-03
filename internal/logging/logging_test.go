package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"  debug  ", slog.LevelDebug},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseLevel(tt.input)
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLookupLevel(t *testing.T) {
	tests := []struct {
		input  string
		want   slog.Level
		wantOK bool
	}{
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"INFO", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"WARN", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"ERROR", slog.LevelError, true},
		{"  debug  ", slog.LevelDebug, true},
		{"", 0, false},
		{"unknown", 0, false},
		{"verbose", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := LookupLevel(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("LookupLevel(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("LookupLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLevelString(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "debug"},
		{slog.LevelInfo, "info"},
		{slog.LevelWarn, "warn"},
		{slog.LevelError, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := LevelString(tt.level)
			if got != tt.want {
				t.Errorf("LevelString(%v) = %q, want %q", tt.level, got, tt.want)
			}
		})
	}
}

func TestPayloadPreview(t *testing.T) {
	short := []byte("hello")
	if got := PayloadPreview(short, 10); got != "hello" {
		t.Errorf("PayloadPreview(short, 10) = %q, want %q", got, "hello")
	}

	long := []byte("abcdefghij")
	if got := PayloadPreview(long, 5); got != "abcde..." {
		t.Errorf("PayloadPreview(long, 5) = %q, want %q", got, "abcde...")
	}

	exact := []byte("abcde")
	if got := PayloadPreview(exact, 5); got != "abcde" {
		t.Errorf("PayloadPreview(exact, 5) = %q, want %q", got, "abcde")
	}
}

func TestInit(t *testing.T) {
	Init("debug")
	if Level.Level() != slog.LevelDebug {
		t.Errorf("after Init(\"debug\"), Level = %v, want %v", Level.Level(), slog.LevelDebug)
	}

	// Verify the default logger uses our level by checking the handler is enabled for debug.
	if !slog.Default().Enabled(nil, slog.LevelDebug) {
		t.Error("after Init(\"debug\"), default logger should be enabled for DEBUG")
	}

	// Reset to info for other tests.
	Init("info")
}
