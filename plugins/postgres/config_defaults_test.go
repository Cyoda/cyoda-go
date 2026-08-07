package postgres

import (
	"strconv"
	"testing"
	"time"
)

// TestConfigVars_DefaultsMatchParseConfig asserts every default advertised in
// ConfigVars() is the value parseConfig actually applies when the var is unset.
//
// Name-level parity is already enforced repo-wide; this is the value-level
// counterpart root vars get from TestRootConfigVars_MatchDefaults and plugin
// vars did not. A documented default that drifts from the code misinforms an
// operator who is reading it precisely because they cannot see the code.
func TestConfigVars_DefaultsMatchParseConfig(t *testing.T) {
	cfg, err := parseConfig(func(k string) string {
		if k == "CYODA_POSTGRES_URL" {
			return "postgres://u:p@localhost:5432/db" // required; not a defaulted var
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseConfig with only the required var set: %v", err)
	}

	actual := map[string]string{
		"CYODA_POSTGRES_MAX_CONNS":                strconv.Itoa(int(cfg.MaxConns)),
		"CYODA_POSTGRES_MIN_CONNS":                strconv.Itoa(int(cfg.MinConns)),
		"CYODA_POSTGRES_MAX_CONN_IDLE_TIME":       cfg.MaxConnIdleTime.String(),
		"CYODA_POSTGRES_AUTO_MIGRATE":             strconv.FormatBool(cfg.AutoMigrate),
		"CYODA_SCHEMA_SAVEPOINT_INTERVAL":         strconv.Itoa(cfg.SchemaSavepointInterval),
		"CYODA_POSTGRES_STATEMENT_TIMEOUT":        cfg.StatementTimeout.String(),
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT":       cfg.IdleInTxTimeout.String(),
		"CYODA_POSTGRES_ACQUIRE_TIMEOUT":          cfg.AcquireTimeout.String(),
		"CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT":     cfg.MigrateLockTimeout.String(),
		"CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT": cfg.SearchStatementTimeout.String(),
	}

	for _, v := range (&plugin{}).ConfigVars() {
		if v.Required {
			continue
		}
		got, ok := actual[v.Name]
		if !ok {
			t.Errorf("%s is advertised in ConfigVars() but this test does not cover its default; add it", v.Name)
			continue
		}
		if !sameDefault(v.Default, got) {
			t.Errorf("%s: ConfigVars() says %q, parseConfig applies %q", v.Name, v.Default, got)
		}
	}
}

// sameDefault compares a ConfigVars() default string against the value
// parseConfig actually produced. A duration can be legitimately spelled two
// ways — the registry favors the compact form ("5m"), while
// time.Duration.String() always renders every component ("5m0s") — so when
// both sides parse as durations, sameDefault compares the parsed values
// rather than the raw strings. When either side does not parse as a
// duration, it falls back to a plain string comparison; this must not
// become so permissive that a real mismatch (e.g. "5m" vs "50m") passes.
func sameDefault(want, got string) bool {
	if want == got {
		return true
	}
	wantDur, wantErr := time.ParseDuration(want)
	gotDur, gotErr := time.ParseDuration(got)
	if wantErr == nil && gotErr == nil {
		return wantDur == gotDur
	}
	return false
}

// TestSameDefault locks in sameDefault's normalization: it must equate the
// two legitimate spellings of the same duration, but must not blur a genuine
// mismatch — including one between two strings that both happen to parse as
// durations (5m vs 50m).
func TestSameDefault(t *testing.T) {
	cases := []struct {
		name       string
		want, got  string
		wantResult bool
	}{
		{"identical strings", "25", "25", true},
		{"duration long form matches short form", "5m", "5m0s", true},
		{"duration short form matches itself rendered long", "30m", "30m0s", true},
		{"non-duration exact mismatch", "25", "26", false},
		{"duration real mismatch", "5m", "50m", false},
		{"duration vs non-duration mismatch", "5m", "banana", false},
		{"bool exact match", "true", "true", true},
		{"bool mismatch", "true", "false", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameDefault(c.want, c.got); got != c.wantResult {
				t.Errorf("sameDefault(%q, %q) = %v, want %v", c.want, c.got, got, c.wantResult)
			}
		})
	}
}
