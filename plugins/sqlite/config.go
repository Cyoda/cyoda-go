package sqlite

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// config holds parsed plugin configuration from CYODA_SQLITE_* env vars.
type config struct {
	Path                    string
	AutoMigrate             bool
	BusyTimeout             time.Duration
	CacheSizeKiB            int
	ReaderPoolSize          int // default GOMAXPROCS clamped to [4,8]; read from CYODA_SQLITE_READER_POOL_SIZE; min 1
	SchemaSavepointInterval int // default 64; read from CYODA_SCHEMA_SAVEPOINT_INTERVAL; min 1
	SchemaExtendMaxRetries  int // default 8; read from CYODA_SCHEMA_EXTEND_MAX_RETRIES; min 1
}

// parseConfig reads CYODA_SQLITE_* env vars via the injected getenv.
func parseConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Path:                    envStringFn(getenv, "CYODA_SQLITE_PATH", DefaultDBPath()),
		AutoMigrate:             envBoolFn(getenv, "CYODA_SQLITE_AUTO_MIGRATE", true),
		BusyTimeout:             envDurationFn(getenv, "CYODA_SQLITE_BUSY_TIMEOUT", 5*time.Second),
		CacheSizeKiB:            envIntFn(getenv, "CYODA_SQLITE_CACHE_SIZE", 64000),
		ReaderPoolSize:          envIntMin1Fn(getenv, "CYODA_SQLITE_READER_POOL_SIZE", defaultReaderPoolSize()),
		SchemaSavepointInterval: envIntMin1Fn(getenv, "CYODA_SCHEMA_SAVEPOINT_INTERVAL", 64),
		SchemaExtendMaxRetries:  envIntMin1Fn(getenv, "CYODA_SCHEMA_EXTEND_MAX_RETRIES", 8),
	}
	if cfg.Path == "" {
		return cfg, fmt.Errorf("CYODA_SQLITE_PATH resolved to empty string")
	}
	return cfg, nil
}

// DefaultDBPath returns the per-OS default path for the sqlite database
// file. Linux and macOS share XDG semantics ($XDG_DATA_HOME/cyoda/cyoda.db,
// fallback ~/.local/share/cyoda/cyoda.db). Windows uses %LocalAppData%\cyoda\
// cyoda.db. Returns the literal "cyoda.db" (current directory) when the user
// home directory cannot be determined.
func DefaultDBPath() string {
	return defaultDBPathResolved(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// defaultDBPathResolved is the testable implementation of DefaultDBPath.
// Injecting goos, getenv, and home makes both OS branches reachable in
// tests regardless of the host platform.
func defaultDBPathResolved(goos string, getenv func(string) string, home func() (string, error)) string {
	if goos == "windows" {
		if local := getenv("LocalAppData"); local != "" {
			return filepath.Join(local, "cyoda", "cyoda.db")
		}
		h, err := home()
		if err != nil {
			return "cyoda.db"
		}
		return filepath.Join(h, "AppData", "Local", "cyoda", "cyoda.db")
	}
	if xdg := getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "cyoda", "cyoda.db")
	}
	h, err := home()
	if err != nil {
		return "cyoda.db"
	}
	return filepath.Join(h, ".local", "share", "cyoda", "cyoda.db")
}

func envStringFn(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntFn(getenv func(string) string, key string, fallback int) int {
	v := getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// envIntMin1Fn reads an integer env var, applies the default when unset
// or invalid, and also applies the default when the value is < 1.
// Used for interval-style config where 0 is not a meaningful value.
func envIntMin1Fn(getenv func(string) string, key string, dflt int) int {
	v := envIntFn(getenv, key, dflt)
	if v < 1 {
		slog.Warn("env var below minimum; using default", "key", key, "value", v, "default", dflt)
		return dflt
	}
	return v
}

func envBoolFn(getenv func(string) string, key string, fallback bool) bool {
	v := getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envDurationFn(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	v := getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
