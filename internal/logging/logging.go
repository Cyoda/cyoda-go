package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Level is the global log level, atomically switchable at runtime.
var Level = &slog.LevelVar{}

// Init configures slog with the global LevelVar and a text handler on stdout.
// Call once at startup before any logging.
func Init(levelStr string) {
	Level.Set(ParseLevel(levelStr))
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: Level,
	})
	slog.SetDefault(slog.New(handler))
}

// LookupLevel converts a string to slog.Level, case-insensitively and
// trimmed. ok is false for anything outside the known set (debug, info,
// warn/warning, error) — the caller decides what an unknown value means
// (ParseLevel defaults it for startup; a request handler should reject it
// rather than substitute a level nobody asked for).
func LookupLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// ParseLevel converts a string to slog.Level. Defaults to INFO for anything
// unrecognised — used only at startup (CYODA_LOG_LEVEL), where a bad value
// falling back to the default is the established behaviour. A request
// handler changing the level at runtime should use LookupLevel instead and
// reject an unknown value rather than silently substitute one.
func ParseLevel(s string) slog.Level {
	if l, ok := LookupLevel(s); ok {
		return l
	}
	return slog.LevelInfo
}

// LevelString returns the lowercase string name of a level.
func LevelString(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	case l <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

// PayloadPreview returns the first maxLen bytes of a payload,
// truncated with "..." if longer. Used for DEBUG-level logging.
func PayloadPreview(payload []byte, maxLen int) string {
	if len(payload) <= maxLen {
		return string(payload)
	}
	return string(payload[:maxLen]) + "..."
}
