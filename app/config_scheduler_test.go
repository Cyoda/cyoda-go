package app

import (
	"strings"
	"testing"
	"time"
)

var schedulerVars = []string{
	"CYODA_SCHEDULER_ENABLED", "CYODA_SCHEDULER_SCAN_INTERVAL", "CYODA_SCHEDULER_MAX_RUNS",
	"CYODA_SCHEDULER_MAX_RUNS_PER_TENANT", "CYODA_SCHEDULER_HEARTBEAT_INTERVAL", "CYODA_SCHEDULER_STALE_AFTER",
	"CYODA_SCHEDULER_MAX_LOST_OWNERS", "CYODA_SCHEDULER_RETRY_DELAY", "CYODA_SCHEDULER_RETRY_DELAY_MAX",
	"CYODA_SCHEDULER_SHUTDOWN_DRAIN",
}

func defaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		Enabled: true, ScanInterval: time.Second, MaxRuns: 8, MaxRunsPerTenant: 4,
		HeartbeatInterval: 15 * time.Second, StaleAfter: 2 * time.Minute, MaxLostOwners: 3,
		RetryDelay: 30 * time.Second, RetryDelayMax: 15 * time.Minute, ShutdownDrain: 20 * time.Second,
	}
}

func TestDefaultConfig_Scheduler(t *testing.T) {
	unsetEnv(t, schedulerVars...)
	if got, want := DefaultConfig().Scheduler, defaultSchedulerConfig(); got != want {
		t.Errorf("Scheduler defaults\n got %+v\nwant %+v", got, want)
	}
}

func TestDefaultConfig_SchedulerEnvOverrides(t *testing.T) {
	for name, value := range map[string]string{
		"CYODA_SCHEDULER_ENABLED": "false", "CYODA_SCHEDULER_SCAN_INTERVAL": "2s",
		"CYODA_SCHEDULER_MAX_RUNS": "16", "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT": "5",
		"CYODA_SCHEDULER_HEARTBEAT_INTERVAL": "5s", "CYODA_SCHEDULER_STALE_AFTER": "3m",
		"CYODA_SCHEDULER_MAX_LOST_OWNERS": "2", "CYODA_SCHEDULER_RETRY_DELAY": "1s",
		"CYODA_SCHEDULER_RETRY_DELAY_MAX": "1m", "CYODA_SCHEDULER_SHUTDOWN_DRAIN": "0s",
	} {
		t.Setenv(name, value)
	}
	want := SchedulerConfig{
		Enabled: false, ScanInterval: 2 * time.Second, MaxRuns: 16, MaxRunsPerTenant: 5,
		HeartbeatInterval: 5 * time.Second, StaleAfter: 3 * time.Minute, MaxLostOwners: 2,
		RetryDelay: time.Second, RetryDelayMax: time.Minute, ShutdownDrain: 0,
	}
	if got := DefaultConfig().Scheduler; got != want {
		t.Errorf("Scheduler overrides\n got %+v\nwant %+v", got, want)
	}
}

func TestValidateScheduler(t *testing.T) {
	if err := ValidateScheduler(defaultSchedulerConfig()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	atMin := defaultSchedulerConfig()
	atMin.StaleAfter = 95 * time.Second // 50s + 3 x 15s
	if err := ValidateScheduler(atMin); err != nil {
		t.Fatalf("STALE_AFTER at its minimum rejected: %v", err)
	}
	noDrain := defaultSchedulerConfig()
	noDrain.ShutdownDrain = 0
	if err := ValidateScheduler(noDrain); err != nil {
		t.Fatalf("a zero shutdown drain rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*SchedulerConfig)
		wantName string
	}{
		{"zero scan interval", func(c *SchedulerConfig) { c.ScanInterval = 0 }, "CYODA_SCHEDULER_SCAN_INTERVAL"},
		{"zero max runs", func(c *SchedulerConfig) { c.MaxRuns = 0 }, "CYODA_SCHEDULER_MAX_RUNS"},
		{"zero per-tenant runs", func(c *SchedulerConfig) { c.MaxRunsPerTenant = 0 }, "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT"},
		{"per-tenant runs above max runs", func(c *SchedulerConfig) { c.MaxRunsPerTenant = 9 }, "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT"},
		{"zero heartbeat interval", func(c *SchedulerConfig) { c.HeartbeatInterval = 0 }, "CYODA_SCHEDULER_HEARTBEAT_INTERVAL"},
		{"stale-after one second below its minimum", func(c *SchedulerConfig) { c.StaleAfter = 94 * time.Second }, "CYODA_SCHEDULER_STALE_AFTER"},
		{"zero max lost owners", func(c *SchedulerConfig) { c.MaxLostOwners = 0 }, "CYODA_SCHEDULER_MAX_LOST_OWNERS"},
		{"zero retry delay", func(c *SchedulerConfig) { c.RetryDelay = 0 }, "CYODA_SCHEDULER_RETRY_DELAY"},
		{"retry delay max below retry delay", func(c *SchedulerConfig) { c.RetryDelayMax = 29 * time.Second }, "CYODA_SCHEDULER_RETRY_DELAY_MAX"},
		{"negative shutdown drain", func(c *SchedulerConfig) { c.ShutdownDrain = -time.Second }, "CYODA_SCHEDULER_SHUTDOWN_DRAIN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := defaultSchedulerConfig()
			tc.mutate(&c)
			err := ValidateScheduler(c)
			if err == nil {
				t.Fatal("ValidateScheduler = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantName+" must") {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}

func TestConfigValidate_RejectsAnInvalidSchedulerSetting(t *testing.T) {
	c := DefaultConfig()
	c.Scheduler.MaxRuns = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CYODA_SCHEDULER_MAX_RUNS") {
		t.Errorf("Config.Validate = %v, want the scheduler setting named", err)
	}
}

func TestSchedulerCalloutDeadlineMax_AtTheDefaults(t *testing.T) {
	unsetEnv(t, "CYODA_RETRY_FIXED_NUM_RETRIES", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS",
		"CYODA_DISPATCH_WAIT_TIMEOUT", "CYODA_CALLOUT_HANDOVER_ALLOWANCE")
	// (1 + 3) x 60s + 5s + 30s
	if got := schedulerCalloutDeadlineMax(DefaultConfig()); got != 275*time.Second {
		t.Errorf("schedulerCalloutDeadlineMax = %s, want 275s", got)
	}
}
