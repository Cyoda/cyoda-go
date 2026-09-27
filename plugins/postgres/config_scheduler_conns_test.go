package postgres

import (
	"strings"
	"testing"
)

// schedulerConnsEnv is a getenv with the required URL and one value for
// CYODA_POSTGRES_SCHEDULER_CONNS ("" = unset).
func schedulerConnsEnv(v string) func(string) string {
	return func(k string) string {
		switch k {
		case "CYODA_POSTGRES_URL":
			return "postgres://test"
		case "CYODA_POSTGRES_SCHEDULER_CONNS":
			return v
		}
		return ""
	}
}

func TestParseConfig_SchedulerConns(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int32
	}{
		{"", 10},
		{"2", 2},
		{"32", 32},
	} {
		cfg, err := parseConfig(schedulerConnsEnv(tc.in))
		if err != nil {
			t.Fatalf("CYODA_POSTGRES_SCHEDULER_CONNS=%q: %v", tc.in, err)
		}
		if cfg.SchedulerConns != tc.want {
			t.Errorf("CYODA_POSTGRES_SCHEDULER_CONNS=%q: SchedulerConns = %d, want %d", tc.in, cfg.SchedulerConns, tc.want)
		}
	}
}

// An invalid value fails startup; it never falls back to the default.
func TestParseConfig_SchedulerConns_InvalidFailsStartup(t *testing.T) {
	for _, in := range []string{"1", "0", "-4", "ten", "2.5", "2147483648"} {
		_, err := parseConfig(schedulerConnsEnv(in))
		if err == nil {
			t.Errorf("CYODA_POSTGRES_SCHEDULER_CONNS=%q was accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), "CYODA_POSTGRES_SCHEDULER_CONNS") {
			t.Errorf("CYODA_POSTGRES_SCHEDULER_CONNS=%q: error %q does not name the variable", in, err)
		}
	}
}

// The fixture configs connect the way a deployment does.
func TestSchedulerConns_FixtureConfigsCarryTheDefault(t *testing.T) {
	if got := defaultStoreConfig().SchedulerConns; got != 10 {
		t.Errorf("defaultStoreConfig().SchedulerConns = %d, want 10", got)
	}
	if got := (DBConfig{URL: "postgres://test"}).toInternal().SchedulerConns; got != 10 {
		t.Errorf("DBConfig.toInternal().SchedulerConns = %d, want 10", got)
	}
}
