package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config holds parsed plugin configuration.
type config struct {
	URL                     string
	MaxConns                int32
	MinConns                int32
	MaxConnIdleTime         time.Duration
	AutoMigrate             bool
	SchemaSavepointInterval int // default 64; read from CYODA_SCHEMA_SAVEPOINT_INTERVAL; min 1

	// StatementTimeout caps a single SQL statement. IdleInTxTimeout caps how
	// long a connection may sit inside an open transaction doing nothing — the
	// one that plugs an abandoned transaction, which is idle by definition. Each
	// *Set field records whether the operator set the var explicitly; see
	// applyCeiling.
	StatementTimeout    time.Duration
	StatementTimeoutSet bool
	IdleInTxTimeout     time.Duration
	IdleInTxTimeoutSet  bool

	// AcquireTimeout bounds the wait for a free pooled connection. It is a
	// Go-side deadline rather than a fourth GUC because pgxpool.Config has no
	// AcquireTimeout field.
	AcquireTimeout time.Duration

	// MigrateLockTimeout bounds a migration's lock waits; SearchStatementTimeout
	// is the async-search path's own statement ceiling.
	MigrateLockTimeout     time.Duration
	SearchStatementTimeout time.Duration
}

// Ceiling defaults. Named here rather than inline so parseConfig,
// defaultStoreConfig and DBConfig.toInternal cannot drift apart — and because
// the same values are quoted in the config.database and STORAGE_UNAVAILABLE
// help topics.
const (
	defaultStatementTimeout       = 5 * time.Minute
	defaultIdleInTxTimeout        = 5 * time.Minute
	defaultAcquireTimeout         = 10 * time.Second
	defaultMigrateLockTimeout     = 5 * time.Minute
	defaultSearchStatementTimeout = 30 * time.Minute
)

// parseConfig reads CYODA_POSTGRES_* env vars via the injected getenv.
// For CYODA_POSTGRES_URL, the _FILE suffix pattern is supported: if
// CYODA_POSTGRES_URL_FILE is set it takes precedence over CYODA_POSTGRES_URL.
func parseConfig(getenv func(string) string) (config, error) {
	url, err := resolveSecretWith(getenv, "CYODA_POSTGRES_URL")
	if err != nil {
		return config{}, err
	}
	cfg := config{
		URL:                     url,
		MaxConns:                envInt32(getenv, "CYODA_POSTGRES_MAX_CONNS", 25),
		MinConns:                envInt32(getenv, "CYODA_POSTGRES_MIN_CONNS", 5),
		MaxConnIdleTime:         envDuration(getenv, "CYODA_POSTGRES_MAX_CONN_IDLE_TIME", 5*time.Minute),
		AutoMigrate:             envBool(getenv, "CYODA_POSTGRES_AUTO_MIGRATE", true),
		SchemaSavepointInterval: envIntMin1(getenv, "CYODA_SCHEMA_SAVEPOINT_INTERVAL", 64),
	}
	if cfg.URL == "" {
		return cfg, fmt.Errorf("CYODA_POSTGRES_URL is required")
	}

	// The ceilings below are the only vars here that reject a malformed value
	// instead of falling back to the default — see envCeiling.
	if cfg.StatementTimeout, cfg.StatementTimeoutSet, err = envCeiling(getenv, "CYODA_POSTGRES_STATEMENT_TIMEOUT", defaultStatementTimeout); err != nil {
		return config{}, err
	}
	if cfg.IdleInTxTimeout, cfg.IdleInTxTimeoutSet, err = envCeiling(getenv, "CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT", defaultIdleInTxTimeout); err != nil {
		return config{}, err
	}
	if cfg.AcquireTimeout, _, err = envCeiling(getenv, "CYODA_POSTGRES_ACQUIRE_TIMEOUT", defaultAcquireTimeout); err != nil {
		return config{}, err
	}
	if cfg.MigrateLockTimeout, _, err = envCeiling(getenv, "CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT", defaultMigrateLockTimeout); err != nil {
		return config{}, err
	}
	if cfg.SearchStatementTimeout, _, err = envCeiling(getenv, "CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT", defaultSearchStatementTimeout); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// Mirrors app.ResolveSecretEnv (separate go.mod; keep behavior in sync).
//
// resolveSecretWith honours the _FILE suffix pattern using the injected getenv
// for the var name lookup, and os.ReadFile for the actual file read.
//
// Precedence: <name>_FILE wins if both are set. Trailing whitespace is trimmed.
// Returns an error if _FILE is set but the file cannot be read.
func resolveSecretWith(getenv func(string) string, name string) (string, error) {
	fileVar := name + "_FILE"
	if path := getenv(fileVar); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("failed to read %s=%q: %w", fileVar, path, err)
		}
		return strings.TrimRight(string(data), " \t\n\r"), nil
	}
	return getenv(name), nil
}

func envInt(getenv func(string) string, key string, dflt int) int {
	v := getenv(key)
	if v == "" {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return dflt
	}
	return n
}

// envInt32 reads an integer env var and narrows to int32 with bounds
// checking. Values outside [math.MinInt32, math.MaxInt32] fall back
// to the default with a logged warning — silent wrap-around on an
// out-of-range value is a CodeQL HIGH (CWE-681) and would produce
// absurd MaxConns/MinConns like -2_147_483_648.
func envInt32(getenv func(string) string, key string, dflt int32) int32 {
	n := envInt(getenv, key, int(dflt))
	if n < math.MinInt32 || n > math.MaxInt32 {
		slog.Warn("env var out of int32 range; using default", "key", key, "value", n, "default", dflt)
		return dflt
	}
	return int32(n)
}

// envIntMin1 reads an integer env var, applies the default when unset
// or invalid, and also applies the default when the value is < 1.
// Used for interval-style config where 0 is not a meaningful value.
func envIntMin1(getenv func(string) string, key string, dflt int) int {
	v := envInt(getenv, key, dflt)
	if v < 1 {
		slog.Warn("env var below minimum; using default", "key", key, "value", v, "default", dflt)
		return dflt
	}
	return v
}

func envBool(getenv func(string) string, key string, dflt bool) bool {
	v := getenv(key)
	if v == "" {
		return dflt
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return dflt
	}
	return b
}

func envDuration(getenv func(string) string, key string, dflt time.Duration) time.Duration {
	v := getenv(key)
	if v == "" {
		return dflt
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return dflt
	}
	return d
}

// newPool creates the pgxpool using the plugin-scoped config.
func newPool(ctx context.Context, cfg config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres URL: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	// Carried in the connection startup packet, so every connection this pool
	// opens is bounded from its first statement — no AfterConnect round-trip,
	// and no window in which a fresh connection is unbounded.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	applyCeiling(poolCfg.ConnConfig.RuntimeParams, "statement_timeout", cfg.StatementTimeout, cfg.StatementTimeoutSet)
	applyCeiling(poolCfg.ConnConfig.RuntimeParams, "idle_in_transaction_session_timeout", cfg.IdleInTxTimeout, cfg.IdleInTxTimeoutSet)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// DBConfig is the exported config type retained for test-fixture callers.
// Production code in the plugin uses the internal config{} directly via
// parseConfig(getenv). Tests can construct a DBConfig, convert to config,
// and call NewPool as a thin wrapper.
type DBConfig struct {
	URL                     string
	MaxConns                int32
	MinConns                int32
	MaxConnIdleTime         string
	AutoMigrate             bool
	SchemaSavepointInterval int // 0 falls back to the internal default (64)
}

func (d DBConfig) toInternal() config {
	idle, _ := time.ParseDuration(d.MaxConnIdleTime)
	if idle == 0 {
		idle = 5 * time.Minute
	}
	interval := d.SchemaSavepointInterval
	if interval < 1 {
		interval = 64
	}
	return config{
		URL: d.URL, MaxConns: d.MaxConns, MinConns: d.MinConns,
		MaxConnIdleTime: idle, AutoMigrate: d.AutoMigrate,
		SchemaSavepointInterval: interval,
		// Fixtures inherit the shipped ceilings rather than zero values, so a
		// fixture-built pool connects the way a deployment does. Zero would read
		// as "disabled" to PostgreSQL — testing a configuration nothing ships.
		StatementTimeout:       defaultStatementTimeout,
		IdleInTxTimeout:        defaultIdleInTxTimeout,
		AcquireTimeout:         defaultAcquireTimeout,
		MigrateLockTimeout:     defaultMigrateLockTimeout,
		SearchStatementTimeout: defaultSearchStatementTimeout,
	}
}

// NewPool is a test-fixture entry point that wraps the internal newPool.
func NewPool(ctx context.Context, cfg DBConfig) (*pgxpool.Pool, error) {
	return newPool(ctx, cfg.toInternal())
}
