package app_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// TestDefaultConfig_StatsGroupMax_ClampsNonPositive pins the contract that
// CYODA_STATS_GROUP_MAX <= 0 is silently clamped to the documented default
// (10000). Without the clamp, plugin GroupedAggregator implementations
// interpret a non-positive cap as "unbounded" (sqlite drops LIMIT, memory
// skips the cardinality check), silently disabling the cardinality
// ceiling — exactly the opposite of what an operator setting "0" intends.
func TestDefaultConfig_StatsGroupMax_ClampsNonPositive(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unparseable (empty) falls back to default", "", 10000},
		{"zero clamps to default", "0", 10000},
		{"negative clamps to default", "-1", 10000},
		{"explicit positive honoured", "250", 250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CYODA_STATS_GROUP_MAX", tc.env)
			cfg := app.DefaultConfig()
			if cfg.StatsGroupMax != tc.want {
				t.Errorf("StatsGroupMax = %d, want %d", cfg.StatsGroupMax, tc.want)
			}
		})
	}
}
