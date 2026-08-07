package postgres

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestPgDurationMillis is coverage row 11a. These values go into the startup
// packet, so a malformed one fails pool.Ping at boot for every deployment.
// PostgreSQL's time units are us/ms/s/min/h/d — "m" is NOT among them — and
// Go's (5*time.Minute).String() is "5m0s", which is also invalid. Bare integer
// milliseconds is the default unit for all three GUCs this renders.
func TestPgDurationMillis(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Minute, "300000"},
		{30 * time.Minute, "1800000"},
		{10 * time.Second, "10000"},
		{time.Millisecond, "1"},
		{0, "0"}, // PostgreSQL's own convention for "disabled"
	}
	for _, tc := range cases {
		if got := pgDurationMillis(tc.in); got != tc.want {
			t.Errorf("pgDurationMillis(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPgDurationMillis_NeverEmitsGoDurationSyntax(t *testing.T) {
	got := pgDurationMillis(5 * time.Minute)
	for _, bad := range []string{"m", "s", "h", "5m0s"} {
		if strings.Contains(got, bad) {
			t.Fatalf("rendered %q contains %q; PostgreSQL rejects it in the startup packet", got, bad)
		}
	}
}

// TestEnvCeiling_RejectsSubMillisecond is coverage row 11g. A value in (0, 1ms)
// truncates to "0", which PostgreSQL reads as DISABLED — the exact inversion of
// intent, so it is rejected rather than silently removing a ceiling.
func TestEnvCeiling_RejectsSubMillisecond(t *testing.T) {
	env := func(string) string { return "500us" }
	if _, _, err := envCeiling(env, "CYODA_POSTGRES_STATEMENT_TIMEOUT", 5*time.Minute); err == nil {
		t.Fatal("sub-millisecond ceiling accepted; it would truncate to 0 and disable the limit")
	}
}

// TestEnvCeiling_SubMillisecondMessageFitsEveryCaller — envCeiling backs both
// the PostgreSQL GUC ceilings and CYODA_POSTGRES_ACQUIRE_TIMEOUT, which is a
// Go-side pool deadline with no server setting behind it. This is shipped
// operator-facing text, so it must not point the operator at a PostgreSQL
// setting that does not exist for the var they actually set.
func TestEnvCeiling_SubMillisecondMessageFitsEveryCaller(t *testing.T) {
	env := func(string) string { return "500us" }
	_, _, err := envCeiling(env, "CYODA_POSTGRES_ACQUIRE_TIMEOUT", defaultAcquireTimeout)
	if err == nil {
		t.Fatal("sub-millisecond acquire timeout accepted")
	}
	if strings.Contains(err.Error(), "PostgreSQL setting") {
		t.Errorf("rejection text claims a PostgreSQL setting the acquire timeout does not have: %v", err)
	}
	// The actionable part must survive the rewording.
	for _, want := range []string{"CYODA_POSTGRES_ACQUIRE_TIMEOUT", "500us", "0 to disable", "at least 1ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection text lost %q: %v", want, err)
		}
	}
}

func TestEnvCeiling_ZeroDisablesExplicitly(t *testing.T) {
	env := func(string) string { return "0" }
	d, set, err := envCeiling(env, "CYODA_POSTGRES_STATEMENT_TIMEOUT", 5*time.Minute)
	if err != nil {
		t.Fatalf("0 must be accepted as an explicit disable: %v", err)
	}
	if d != 0 || !set {
		t.Fatalf("got (%v, %v), want (0, true)", d, set)
	}
}

func TestEnvCeiling_UnsetReportsNotExplicit(t *testing.T) {
	d, set, err := envCeiling(func(string) string { return "" }, "X", 5*time.Minute)
	if err != nil || d != 5*time.Minute || set {
		t.Fatalf("got (%v, %v, %v), want (5m, false, nil)", d, set, err)
	}
}

func TestEnvCeiling_RejectsMalformed(t *testing.T) {
	if _, _, err := envCeiling(func(string) string { return "banana" }, "X", time.Minute); err == nil {
		t.Fatal("malformed duration accepted")
	}
}

// TestEnvCeiling_RejectsNegative — a negative ceiling is not a value PostgreSQL
// accepts, and truncating or clamping it would substitute a limit the operator
// did not ask for.
func TestEnvCeiling_RejectsNegative(t *testing.T) {
	if _, _, err := envCeiling(func(string) string { return "-1s" }, "X", time.Minute); err == nil {
		t.Fatal("negative ceiling accepted")
	}
}

// TestApplyCeilings_Precedence is coverage row 11h.
func TestApplyCeilings_Precedence(t *testing.T) {
	t.Run("neither set — the documented default applies", func(t *testing.T) {
		params := map[string]string{}
		applyCeiling(params, "statement_timeout", 5*time.Minute, false)
		if params["statement_timeout"] != "300000" {
			t.Fatalf("default not applied: %v", params)
		}
	})
	t.Run("DSN only — the operator's value survives", func(t *testing.T) {
		params := map[string]string{"statement_timeout": "90000"}
		applyCeiling(params, "statement_timeout", 5*time.Minute, false)
		if params["statement_timeout"] != "90000" {
			t.Fatalf("a default the operator never set overrode their DSN value: %v", params)
		}
	})
	t.Run("both set — the env var wins", func(t *testing.T) {
		params := map[string]string{"statement_timeout": "90000"}
		applyCeiling(params, "statement_timeout", 2*time.Minute, true)
		if params["statement_timeout"] != "120000" {
			t.Fatalf("explicit env var did not win: %v", params)
		}
	})
}

// TestApplyCeiling_OverrideWarns pins the operator-visible half of row 11h: an
// override is silent otherwise, and an operator whose DSN value stopped taking
// effect has nothing to go on. The DSN itself is never logged — only the
// setting name and the two rendered values (Gate 3).
func TestApplyCeiling_OverrideWarns(t *testing.T) {
	logged := captureSlog(t, func() {
		applyCeiling(map[string]string{"statement_timeout": "90000"}, "statement_timeout", 2*time.Minute, true)
	})
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("override was not logged at WARN: %q", logged)
	}
	for _, want := range []string{"statement_timeout", "90000", "120000"} {
		if !strings.Contains(logged, want) {
			t.Errorf("WARN line does not mention %q: %q", want, logged)
		}
	}
}

// TestApplyCeiling_NoOverrideIsSilent — the two non-override cases are normal
// operation, so they must not produce a warning an operator has to triage.
func TestApplyCeiling_NoOverrideIsSilent(t *testing.T) {
	logged := captureSlog(t, func() {
		applyCeiling(map[string]string{}, "statement_timeout", 5*time.Minute, false)
		applyCeiling(map[string]string{"statement_timeout": "90000"}, "statement_timeout", 5*time.Minute, false)
		applyCeiling(map[string]string{}, "statement_timeout", 2*time.Minute, true)
	})
	if strings.Contains(logged, "level=WARN") {
		t.Fatalf("a non-override case warned: %q", logged)
	}
}

// captureSlog swaps the default slog logger for one writing into a buffer,
// runs fn, restores the previous logger and returns what was written.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// --- parseConfig: a malformed ceiling fails closed -------------------------

// TestParseConfig_MalformedCeiling_IsAnError is the fail-closed half. The
// neighbouring envInt/envDuration/envBool helpers silently fall back to their
// default, which for a ceiling would mean silently removing a safety limit, so
// these five vars reject instead.
func TestParseConfig_MalformedCeiling_IsAnError(t *testing.T) {
	for _, key := range []string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT",
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT",
		"CYODA_POSTGRES_ACQUIRE_TIMEOUT",
		"CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT",
		"CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT",
	} {
		t.Run(key, func(t *testing.T) {
			getenv := func(k string) string {
				switch k {
				case "CYODA_POSTGRES_URL":
					return "postgres://test"
				case key:
					return "banana"
				}
				return ""
			}
			if _, err := parseConfig(getenv); err == nil {
				t.Fatalf("%s=banana was accepted; a malformed ceiling must fail closed", key)
			}
		})
	}
}

// TestParseConfig_CeilingDefaults pins the shipped defaults. They are named in
// the STORAGE_UNAVAILABLE help topic and in config.database, so drifting them
// here makes shipped documentation wrong.
func TestParseConfig_CeilingDefaults(t *testing.T) {
	cfg, err := parseConfig(func(k string) string {
		if k == "CYODA_POSTGRES_URL" {
			return "postgres://test"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.StatementTimeout != 5*time.Minute || cfg.StatementTimeoutSet {
		t.Errorf("StatementTimeout = (%v, set=%v), want (5m, false)", cfg.StatementTimeout, cfg.StatementTimeoutSet)
	}
	if cfg.IdleInTxTimeout != 5*time.Minute || cfg.IdleInTxTimeoutSet {
		t.Errorf("IdleInTxTimeout = (%v, set=%v), want (5m, false)", cfg.IdleInTxTimeout, cfg.IdleInTxTimeoutSet)
	}
	if cfg.AcquireTimeout != 10*time.Second {
		t.Errorf("AcquireTimeout = %v, want 10s", cfg.AcquireTimeout)
	}
	if cfg.MigrateLockTimeout != 5*time.Minute {
		t.Errorf("MigrateLockTimeout = %v, want 5m", cfg.MigrateLockTimeout)
	}
	if cfg.SearchStatementTimeout != 30*time.Minute {
		t.Errorf("SearchStatementTimeout = %v, want 30m", cfg.SearchStatementTimeout)
	}
}

// TestParseConfig_CeilingExplicitSetIsRecorded — applyCeiling's DSN-deference
// depends on this flag, so parseConfig must carry it through rather than
// inferring "set" from a non-default value.
func TestParseConfig_CeilingExplicitSetIsRecorded(t *testing.T) {
	cfg, err := parseConfig(func(k string) string {
		switch k {
		case "CYODA_POSTGRES_URL":
			return "postgres://test"
		case "CYODA_POSTGRES_STATEMENT_TIMEOUT":
			return "5m" // the default value, but explicitly set
		case "CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT":
			return "0" // explicitly disabled
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.StatementTimeout != 5*time.Minute || !cfg.StatementTimeoutSet {
		t.Errorf("StatementTimeout = (%v, set=%v), want (5m, true)", cfg.StatementTimeout, cfg.StatementTimeoutSet)
	}
	if cfg.IdleInTxTimeout != 0 || !cfg.IdleInTxTimeoutSet {
		t.Errorf("IdleInTxTimeout = (%v, set=%v), want (0, true)", cfg.IdleInTxTimeout, cfg.IdleInTxTimeoutSet)
	}
}

// TestDBConfigToInternal_InheritsCeilingDefaults — DBConfig is the test-fixture
// entry point into newPool. Zero ceilings there would mean fixtures connect with
// every limit disabled, i.e. testing a configuration nothing ships with.
func TestDBConfigToInternal_InheritsCeilingDefaults(t *testing.T) {
	cfg := DBConfig{URL: "postgres://test"}.toInternal()
	if cfg.StatementTimeout != 5*time.Minute {
		t.Errorf("StatementTimeout = %v, want 5m", cfg.StatementTimeout)
	}
	if cfg.IdleInTxTimeout != 5*time.Minute {
		t.Errorf("IdleInTxTimeout = %v, want 5m", cfg.IdleInTxTimeout)
	}
	if cfg.AcquireTimeout != 10*time.Second {
		t.Errorf("AcquireTimeout = %v, want 10s", cfg.AcquireTimeout)
	}
	if cfg.MigrateLockTimeout != 5*time.Minute {
		t.Errorf("MigrateLockTimeout = %v, want 5m", cfg.MigrateLockTimeout)
	}
	if cfg.SearchStatementTimeout != 30*time.Minute {
		t.Errorf("SearchStatementTimeout = %v, want 30m", cfg.SearchStatementTimeout)
	}
}

// TestDefaultStoreConfig_MatchesParseConfigCeilings — defaultStoreConfig is the
// "no knobs touched" baseline and documents itself as matching parseConfig. A
// zero ceiling there reads as "disabled" to every consumer of the field.
func TestDefaultStoreConfig_MatchesParseConfigCeilings(t *testing.T) {
	parsed, err := parseConfig(func(k string) string {
		if k == "CYODA_POSTGRES_URL" {
			return "postgres://test"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	got := defaultStoreConfig()
	if got.StatementTimeout != parsed.StatementTimeout ||
		got.IdleInTxTimeout != parsed.IdleInTxTimeout ||
		got.AcquireTimeout != parsed.AcquireTimeout ||
		got.MigrateLockTimeout != parsed.MigrateLockTimeout ||
		got.SearchStatementTimeout != parsed.SearchStatementTimeout {
		t.Errorf("defaultStoreConfig ceilings = %+v, want parseConfig's %+v", got, parsed)
	}
}
