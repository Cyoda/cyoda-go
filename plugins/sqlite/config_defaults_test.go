package sqlite

import (
	"strconv"
	"testing"
	"time"
)

// describedNotLiteral holds ConfigVars() names whose Default field documents a
// formula rather than a single literal value, so it cannot be compared against
// one parseConfig() run with a plain string/duration comparison — the real
// default is host- or OS-dependent. Skipped here, not uncovered: each formula
// is pinned by its own test.
//
//   - CYODA_SQLITE_PATH ("$XDG_DATA_HOME/cyoda/cyoda.db (Windows:
//     %LocalAppData%\cyoda\cyoda.db)") — TestDefaultDBPathResolved_* in
//     config_test.go pins every OS/env branch against the exact join.
//   - CYODA_SQLITE_READER_POOL_SIZE ("GOMAXPROCS clamped to 4..8") —
//     TestReaderPoolSize_ConfiguredFromEnv in reader_pool_size_test.go pins the
//     unset case against defaultReaderPoolSize() itself.
var describedNotLiteral = map[string]bool{
	"CYODA_SQLITE_PATH":             true,
	"CYODA_SQLITE_READER_POOL_SIZE": true,
}

// TestConfigVars_DefaultsMatchParseConfig asserts every default advertised in
// ConfigVars() is the value parseConfig actually applies when the var is unset.
//
// Name-level parity is already enforced repo-wide; this is the value-level
// counterpart root vars get from TestRootConfigVars_MatchDefaults and plugin
// vars did not. A documented default that drifts from the code misinforms an
// operator who is reading it precisely because they cannot see the code.
func TestConfigVars_DefaultsMatchParseConfig(t *testing.T) {
	cfg, err := parseConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("parseConfig with nothing set: %v", err)
	}

	actual := map[string]string{
		"CYODA_SQLITE_AUTO_MIGRATE":       strconv.FormatBool(cfg.AutoMigrate),
		"CYODA_SQLITE_BUSY_TIMEOUT":       cfg.BusyTimeout.String(),
		"CYODA_SQLITE_CACHE_SIZE":         strconv.Itoa(cfg.CacheSizeKiB),
		"CYODA_SQLITE_SEARCH_SCAN_LIMIT":  strconv.Itoa(cfg.SearchScanLimit),
		"CYODA_SCHEMA_SAVEPOINT_INTERVAL": strconv.Itoa(cfg.SchemaSavepointInterval),
		"CYODA_SCHEMA_EXTEND_MAX_RETRIES": strconv.Itoa(cfg.SchemaExtendMaxRetries),
	}

	for _, v := range (&plugin{}).ConfigVars() {
		if v.Required || describedNotLiteral[v.Name] {
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
		{"identical strings", "64", "64", true},
		{"duration long form matches short form", "5s", "5s", true},
		{"duration short form matches itself rendered long", "1m", "1m0s", true},
		{"non-duration exact mismatch", "64", "65", false},
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
