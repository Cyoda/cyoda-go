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
