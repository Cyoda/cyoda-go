package app

import (
	"testing"
	"time"
)

// validConfig is a Config carrying only the fields Config.Validate inspects,
// all set to accepted values — the baseline each case below perturbs by
// exactly one field.
func validConfig() Config {
	return Config{
		SearchAsync:                SearchAsyncConfig{Workers: 8, QueueLen: 256, MaxPerTenant: 8},
		SearchJobHeartbeatInterval: 15 * time.Second,
		SearchJobStaleAfter:        5 * time.Minute,
		SearchJobMaxAttempts:       3,
		GRPC:                       GRPCConfig{KeepAliveInterval: 10, KeepAliveTimeout: 30},
		Callout:                    validCalloutConfig(),
		Cluster:                    validDispatchConfig(),
	}
}

// TestConfig_Validate pins that the async-search invariants are enforced by
// the package that DEPENDS on them, not only by the binary's startup path.
// app.New builds the worker pool straight from cfg.SearchAsync and
// NewWorkerPool explicitly delegates validation to its caller, so an
// in-process embedder (internal/e2e, any test harness) that skipped
// cmd/cyoda/main.go's checks used to reach make(chan jobFunc, -1) and panic.
func TestConfig_Validate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
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
		{"zero max attempts", func(c *Config) { c.SearchJobMaxAttempts = 0 }},
		{"answer limit above its upper bound", func(c *Config) { c.Callout.ResponseTimeout = 2 * time.Minute }},
		{"negative patience", func(c *Config) { c.Cluster.DispatchWaitTimeout = -time.Second }},
		{"zero pass allowance", func(c *Config) { c.Callout.PassAllowance = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", tc.name)
			}
		})
	}
}

func TestValidateGRPCKeepAlive_RejectsNonPositive(t *testing.T) {
	for _, c := range []GRPCConfig{{KeepAliveInterval: 0, KeepAliveTimeout: 30}, {KeepAliveInterval: 10, KeepAliveTimeout: -1}} {
		if err := ValidateGRPCKeepAlive(c); err == nil {
			t.Errorf("ValidateGRPCKeepAlive(%+v) = nil, want error", c)
		}
	}
	if err := ValidateGRPCKeepAlive(GRPCConfig{KeepAliveInterval: 10, KeepAliveTimeout: 30}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestValidateHTTP_RejectsNegative(t *testing.T) {
	if err := ValidateHTTP(HTTPConfig{ReadTimeout: -1}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if err := ValidateHTTP(HTTPConfig{}); err != nil {
		t.Fatalf("all-zero (disabled) rejected: %v", err)
	}
}

func TestValidateSearchJobMaxAttempts(t *testing.T) {
	if err := ValidateSearchJobMaxAttempts(1); err != nil {
		t.Fatalf("1 must be valid: %v", err)
	}
	if err := ValidateSearchJobMaxAttempts(0); err == nil {
		t.Fatal("0 must be rejected")
	}
}
