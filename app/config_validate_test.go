package app

import (
	"testing"
	"time"
)

// validSearchConfig is a Config carrying only the fields Config.Validate
// inspects, all set to accepted values — the baseline each case below
// perturbs by exactly one field.
func validSearchConfig() Config {
	return Config{
		SearchAsync:                SearchAsyncConfig{Workers: 8, QueueLen: 256, MaxPerTenant: 8},
		SearchJobHeartbeatInterval: 15 * time.Second,
		SearchJobStaleAfter:        5 * time.Minute,
	}
}

// TestConfig_Validate pins that the async-search invariants are enforced by
// the package that DEPENDS on them, not only by the binary's startup path.
// app.New builds the worker pool straight from cfg.SearchAsync and
// NewWorkerPool explicitly delegates validation to its caller, so an
// in-process embedder (internal/e2e, any test harness) that skipped
// cmd/cyoda/main.go's checks used to reach make(chan jobFunc, -1) and panic.
func TestConfig_Validate(t *testing.T) {
	if err := validSearchConfig().Validate(); err != nil {
		t.Fatalf("Validate() on a valid config = %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero workers", func(c *Config) { c.SearchAsync.Workers = 0 }},
		{"negative queue", func(c *Config) { c.SearchAsync.QueueLen = -1 }},
		{"negative per-tenant cap", func(c *Config) { c.SearchAsync.MaxPerTenant = -1 }},
		{"non-positive heartbeat", func(c *Config) { c.SearchJobHeartbeatInterval = 0 }},
		{"stale-after too close to heartbeat", func(c *Config) { c.SearchJobStaleAfter = 20 * time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validSearchConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", tc.name)
			}
		})
	}
}
