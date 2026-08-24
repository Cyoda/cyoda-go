package app

import (
	"os"
	"testing"
	"time"
)

func TestDefaultConfig_SearchMaxSortKeys(t *testing.T) {
	t.Setenv("CYODA_SEARCH_MAX_SORT_KEYS", "")
	if got := DefaultConfig().SearchMaxSortKeys; got != 16 {
		t.Fatalf("default SearchMaxSortKeys = %d, want 16", got)
	}
	t.Setenv("CYODA_SEARCH_MAX_SORT_KEYS", "4")
	if got := DefaultConfig().SearchMaxSortKeys; got != 4 {
		t.Fatalf("env SearchMaxSortKeys = %d, want 4", got)
	}
	// The <=0 guard re-defaults to 16: a zero or negative cap would 400
	// every sorted request. Removing the guard must cause these to fail.
	t.Setenv("CYODA_SEARCH_MAX_SORT_KEYS", "0")
	if got := DefaultConfig().SearchMaxSortKeys; got != 16 {
		t.Fatalf("SearchMaxSortKeys(0) = %d, want 16 (<=0 guard)", got)
	}
	t.Setenv("CYODA_SEARCH_MAX_SORT_KEYS", "-3")
	if got := DefaultConfig().SearchMaxSortKeys; got != 16 {
		t.Fatalf("SearchMaxSortKeys(-3) = %d, want 16 (<=0 guard)", got)
	}
}

// TestDefaultConfig_SearchAsync asserts the CYODA_SEARCH_ASYNC_WORKERS /
// CYODA_SEARCH_ASYNC_QUEUE defaults (8 / 256) under an empty environment.
func TestDefaultConfig_SearchAsync(t *testing.T) {
	t.Setenv("CYODA_SEARCH_ASYNC_WORKERS", "")
	os.Unsetenv("CYODA_SEARCH_ASYNC_WORKERS")
	t.Setenv("CYODA_SEARCH_ASYNC_QUEUE", "")
	os.Unsetenv("CYODA_SEARCH_ASYNC_QUEUE")

	cfg := DefaultConfig()
	if cfg.SearchAsync.Workers != 8 {
		t.Errorf("default SearchAsync.Workers = %d, want 8", cfg.SearchAsync.Workers)
	}
	if cfg.SearchAsync.QueueLen != 256 {
		t.Errorf("default SearchAsync.QueueLen = %d, want 256", cfg.SearchAsync.QueueLen)
	}
}

// TestDefaultConfig_SearchAsyncEnvOverride confirms both vars actually bind
// through envInt rather than being hardcoded.
func TestDefaultConfig_SearchAsyncEnvOverride(t *testing.T) {
	t.Setenv("CYODA_SEARCH_ASYNC_WORKERS", "4")
	t.Setenv("CYODA_SEARCH_ASYNC_QUEUE", "64")

	cfg := DefaultConfig()
	if cfg.SearchAsync.Workers != 4 {
		t.Errorf("SearchAsync.Workers override = %d, want 4", cfg.SearchAsync.Workers)
	}
	if cfg.SearchAsync.QueueLen != 64 {
		t.Errorf("SearchAsync.QueueLen override = %d, want 64", cfg.SearchAsync.QueueLen)
	}
}

// TestSearchAsyncConfig_ValidateRejectsInvalid asserts workers < 1 and
// queue < 0 are hard startup errors — config is a QA'd artefact, not
// runtime input to clamp defensively.
func TestSearchAsyncConfig_ValidateRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		cfg  SearchAsyncConfig
	}{
		{"zero workers", SearchAsyncConfig{Workers: 0, QueueLen: 256}},
		{"negative workers", SearchAsyncConfig{Workers: -1, QueueLen: 256}},
		{"negative queue", SearchAsyncConfig{Workers: 8, QueueLen: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSearchAsync(tc.cfg); err == nil {
				t.Fatalf("ValidateSearchAsync(%+v) = nil, want an error", tc.cfg)
			}
		})
	}
}

// TestSearchAsyncConfig_ValidateAcceptsValid asserts the documented default
// and its floor (1 worker, unbuffered queue) are both accepted.
func TestSearchAsyncConfig_ValidateAcceptsValid(t *testing.T) {
	cases := []SearchAsyncConfig{
		{Workers: 1, QueueLen: 0},
		{Workers: 8, QueueLen: 256},
	}
	for _, c := range cases {
		if err := ValidateSearchAsync(c); err != nil {
			t.Errorf("ValidateSearchAsync(%+v) = %v, want nil", c, err)
		}
	}
}

// TestDefaultConfig_SearchJobHeartbeatInterval asserts the
// CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL default (15s) under an empty
// environment, and that an env override binds through envDuration.
func TestDefaultConfig_SearchJobHeartbeatInterval(t *testing.T) {
	t.Setenv("CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL", "")
	os.Unsetenv("CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL")

	if got := DefaultConfig().SearchJobHeartbeatInterval; got != 15*time.Second {
		t.Errorf("default SearchJobHeartbeatInterval = %s, want 15s", got)
	}

	t.Setenv("CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL", "3s")
	if got := DefaultConfig().SearchJobHeartbeatInterval; got != 3*time.Second {
		t.Errorf("env SearchJobHeartbeatInterval = %s, want 3s", got)
	}
}

// TestValidateSearchJobHeartbeat_RejectsNonPositive asserts the hard
// startup failure for a zero or negative interval — time.NewTicker panics
// on such a value, so this guards a startup crash, not just a slow default.
func TestValidateSearchJobHeartbeat_RejectsNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -1 * time.Second} {
		if err := ValidateSearchJobHeartbeat(d); err == nil {
			t.Errorf("ValidateSearchJobHeartbeat(%s) = nil, want an error", d)
		}
	}
}

func TestValidateSearchJobHeartbeat_AcceptsPositive(t *testing.T) {
	for _, d := range []time.Duration{time.Millisecond, 15 * time.Second, time.Hour} {
		if err := ValidateSearchJobHeartbeat(d); err != nil {
			t.Errorf("ValidateSearchJobHeartbeat(%s) = %v, want nil", d, err)
		}
	}
}

// TestDefaultConfig_SearchJobStaleAfter asserts the
// CYODA_SEARCH_JOB_STALE_AFTER default (5m) under an empty environment, and
// that an env override binds through envDuration.
func TestDefaultConfig_SearchJobStaleAfter(t *testing.T) {
	t.Setenv("CYODA_SEARCH_JOB_STALE_AFTER", "")
	os.Unsetenv("CYODA_SEARCH_JOB_STALE_AFTER")

	if got := DefaultConfig().SearchJobStaleAfter; got != 5*time.Minute {
		t.Errorf("default SearchJobStaleAfter = %s, want 5m", got)
	}

	t.Setenv("CYODA_SEARCH_JOB_STALE_AFTER", "10m")
	if got := DefaultConfig().SearchJobStaleAfter; got != 10*time.Minute {
		t.Errorf("env SearchJobStaleAfter = %s, want 10m", got)
	}
}

// TestValidateSearchJobStaleAfter_RejectsBelowFourXInterval asserts the
// spec's interval « staleAfter invariant is a hard startup error once made
// checkable as staleAfter >= 4x interval, not silently accepted.
func TestValidateSearchJobStaleAfter_RejectsBelowFourXInterval(t *testing.T) {
	cases := []struct {
		name       string
		staleAfter time.Duration
		interval   time.Duration
	}{
		{"equal to interval", 15 * time.Second, 15 * time.Second},
		{"just under 4x", 59 * time.Second, 15 * time.Second},
		{"documented defaults inverted", 15 * time.Second, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSearchJobStaleAfter(tc.staleAfter, tc.interval); err == nil {
				t.Errorf("ValidateSearchJobStaleAfter(%s, %s) = nil, want an error", tc.staleAfter, tc.interval)
			}
		})
	}
}

// TestValidateSearchJobStaleAfter_AcceptsAtOrAboveFourXInterval asserts the
// boundary (exactly 4x) and the documented defaults (5m staleAfter, 15s
// interval = 20x) both pass.
func TestValidateSearchJobStaleAfter_AcceptsAtOrAboveFourXInterval(t *testing.T) {
	cases := []struct {
		name       string
		staleAfter time.Duration
		interval   time.Duration
	}{
		{"exactly 4x", 60 * time.Second, 15 * time.Second},
		{"documented defaults", 5 * time.Minute, 15 * time.Second},
		{"well above 4x", time.Hour, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSearchJobStaleAfter(tc.staleAfter, tc.interval); err != nil {
				t.Errorf("ValidateSearchJobStaleAfter(%s, %s) = %v, want nil", tc.staleAfter, tc.interval, err)
			}
		})
	}
}

func TestDefaultConfig_OIDCDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.IAM.OIDC.RequireHTTPS {
		t.Error("default RequireHTTPS should be true")
	}
	if cfg.IAM.OIDC.ConnectTimeout != 5*time.Second {
		t.Errorf("default ConnectTimeout = %v, want 5s", cfg.IAM.OIDC.ConnectTimeout)
	}
	if cfg.IAM.OIDC.SocketTimeout != 5*time.Second {
		t.Errorf("default SocketTimeout = %v, want 5s", cfg.IAM.OIDC.SocketTimeout)
	}
	if cfg.IAM.OIDC.ConnectionRequestTimeout != 5*time.Second {
		t.Errorf("default ConnectionRequestTimeout = %v, want 5s", cfg.IAM.OIDC.ConnectionRequestTimeout)
	}
	if cfg.IAM.OIDC.AllowPrivateNetworks {
		t.Error("default AllowPrivateNetworks should be false")
	}
	if cfg.IAM.OIDC.DefaultRolesClaim != "roles" {
		t.Errorf("default DefaultRolesClaim = %q, want roles", cfg.IAM.OIDC.DefaultRolesClaim)
	}
}

func TestDefaultConfig_OIDCEnvOverrides(t *testing.T) {
	t.Setenv("CYODA_OIDC_REQUIRE_HTTPS", "false")
	t.Setenv("CYODA_OIDC_CONNECT_TIMEOUT_MS", "1000")
	t.Setenv("CYODA_OIDC_SOCKET_TIMEOUT_MS", "2000")
	t.Setenv("CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS", "3000")
	t.Setenv("CYODA_OIDC_ALLOW_PRIVATE_NETWORKS", "true")
	t.Setenv("CYODA_OIDC_ROLES_CLAIM", "cognito:groups")

	cfg := DefaultConfig()
	if cfg.IAM.OIDC.RequireHTTPS {
		t.Error("RequireHTTPS override failed")
	}
	if cfg.IAM.OIDC.ConnectTimeout != time.Second {
		t.Errorf("ConnectTimeout = %v, want 1s", cfg.IAM.OIDC.ConnectTimeout)
	}
	if cfg.IAM.OIDC.SocketTimeout != 2*time.Second {
		t.Errorf("SocketTimeout = %v, want 2s", cfg.IAM.OIDC.SocketTimeout)
	}
	if cfg.IAM.OIDC.ConnectionRequestTimeout != 3*time.Second {
		t.Errorf("ConnectionRequestTimeout = %v, want 3s", cfg.IAM.OIDC.ConnectionRequestTimeout)
	}
	if !cfg.IAM.OIDC.AllowPrivateNetworks {
		t.Error("AllowPrivateNetworks override failed")
	}
	if cfg.IAM.OIDC.DefaultRolesClaim != "cognito:groups" {
		t.Errorf("DefaultRolesClaim = %q, want cognito:groups", cfg.IAM.OIDC.DefaultRolesClaim)
	}
}

// TestDefaultConfig_Scheduler asserts the seven CYODA_SCHEDULER_* defaults
// (design doc §9 / plan Task D4) under an empty environment.
func TestDefaultConfig_Scheduler(t *testing.T) {
	for _, v := range []string{
		"CYODA_SCHEDULER_ENABLED",
		"CYODA_SCHEDULER_SCAN_INTERVAL",
		"CYODA_SCHEDULER_BATCH_SIZE",
		"CYODA_SCHEDULER_DISTRIBUTION",
		"CYODA_SCHEDULER_COORDINATOR",
		"CYODA_SCHEDULER_REDISPATCH_BACKOFF",
		"CYODA_SCHEDULER_EXPIRY_GRACE",
	} {
		// t.Setenv registers test-scoped restoration; os.Unsetenv then
		// actually removes the var for this test body — envString returns
		// "" (not its fallback) for a var that is set-but-empty, so a plain
		// t.Setenv(v, "") would defeat the Distribution/Coordinator defaults.
		t.Setenv(v, "")
		os.Unsetenv(v)
	}

	c := DefaultConfig()
	if !c.Scheduler.Enabled {
		t.Error("Scheduler.Enabled default should be true")
	}
	if c.Scheduler.ScanInterval != time.Second {
		t.Errorf("Scheduler.ScanInterval = %v, want 1s", c.Scheduler.ScanInterval)
	}
	if c.Scheduler.BatchSize != 100 {
		t.Errorf("Scheduler.BatchSize = %d, want 100", c.Scheduler.BatchSize)
	}
	if c.Scheduler.Distribution != "round-robin" {
		t.Errorf("Scheduler.Distribution = %q, want round-robin", c.Scheduler.Distribution)
	}
	if c.Scheduler.Coordinator != "lowest-node-id" {
		t.Errorf("Scheduler.Coordinator = %q, want lowest-node-id", c.Scheduler.Coordinator)
	}
	if c.Scheduler.RedispatchBackoff != 30*time.Second {
		t.Errorf("Scheduler.RedispatchBackoff = %v, want 30s", c.Scheduler.RedispatchBackoff)
	}
	if c.Scheduler.ExpiryGrace != 100*time.Millisecond {
		t.Errorf("Scheduler.ExpiryGrace = %v, want 100ms", c.Scheduler.ExpiryGrace)
	}
}

func TestDefaultConfig_AuthCacheReconcileInterval(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.IAM.AuthCacheReconcileInterval != 60*time.Second {
		t.Fatalf("default AuthCacheReconcileInterval: want 60s, got %s", cfg.IAM.AuthCacheReconcileInterval)
	}
}

func TestValidateIAM_ReconcileIntervalFloor(t *testing.T) {
	iam := DefaultConfig().IAM
	iam.AuthCacheReconcileInterval = 500 * time.Millisecond
	if err := ValidateIAM(iam); err == nil {
		t.Fatal("sub-second reconcile interval must be rejected")
	}
	// Rejected in mock mode too — a bad explicit value is a config error
	// regardless of mode.
	iam.Mode = "mock"
	if err := ValidateIAM(iam); err == nil {
		t.Fatal("sub-second reconcile interval must be rejected in mock mode")
	}
	iam.AuthCacheReconcileInterval = time.Second
	iam.Mode = "mock"
	if err := ValidateIAM(iam); err != nil {
		t.Fatalf("1s interval must be accepted: %v", err)
	}
}

// TestDefaultConfig_SchedulerEnvOverrides confirms each var actually binds
// through envBool/envDuration/envInt/envString rather than being hardcoded.
func TestDefaultConfig_SchedulerEnvOverrides(t *testing.T) {
	t.Setenv("CYODA_SCHEDULER_ENABLED", "false")
	t.Setenv("CYODA_SCHEDULER_SCAN_INTERVAL", "2s")
	t.Setenv("CYODA_SCHEDULER_BATCH_SIZE", "50")
	t.Setenv("CYODA_SCHEDULER_DISTRIBUTION", "self")
	t.Setenv("CYODA_SCHEDULER_COORDINATOR", "lowest-node-id")
	t.Setenv("CYODA_SCHEDULER_REDISPATCH_BACKOFF", "1m")
	t.Setenv("CYODA_SCHEDULER_EXPIRY_GRACE", "250ms")

	c := DefaultConfig()
	if c.Scheduler.Enabled {
		t.Error("Scheduler.Enabled override failed")
	}
	if c.Scheduler.ScanInterval != 2*time.Second {
		t.Errorf("Scheduler.ScanInterval = %v, want 2s", c.Scheduler.ScanInterval)
	}
	if c.Scheduler.BatchSize != 50 {
		t.Errorf("Scheduler.BatchSize = %d, want 50", c.Scheduler.BatchSize)
	}
	if c.Scheduler.Distribution != "self" {
		t.Errorf("Scheduler.Distribution = %q, want self", c.Scheduler.Distribution)
	}
	if c.Scheduler.RedispatchBackoff != time.Minute {
		t.Errorf("Scheduler.RedispatchBackoff = %v, want 1m", c.Scheduler.RedispatchBackoff)
	}
	if c.Scheduler.ExpiryGrace != 250*time.Millisecond {
		t.Errorf("Scheduler.ExpiryGrace = %v, want 250ms", c.Scheduler.ExpiryGrace)
	}
}
