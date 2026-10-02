package app

import (
	"os"
	"runtime"
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

func TestDefaultConfig_MockKindIsService(t *testing.T) {
	t.Setenv("CYODA_IAM_MOCK_KIND", "") // restored after the test
	os.Unsetenv("CYODA_IAM_MOCK_KIND")
	if got := DefaultConfig().IAM.MockKind; got != "service" {
		t.Fatalf("default IAM.MockKind = %q, want service", got)
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

func TestDefaultConfig_TokenEndpointCost(t *testing.T) {
	for _, k := range []string{"CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE", "CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS"} {
		t.Setenv(k, "") // restored after the test
		os.Unsetenv(k)
	}
	iam := DefaultConfig().IAM
	if iam.TokenRequestsPerMinute != 600 {
		t.Errorf("default TokenRequestsPerMinute = %d, want 600", iam.TokenRequestsPerMinute)
	}
	if iam.TokenMaxConcurrentSecretChecks != runtime.GOMAXPROCS(0) {
		t.Errorf("default TokenMaxConcurrentSecretChecks = %d, want %d (GOMAXPROCS)", iam.TokenMaxConcurrentSecretChecks, runtime.GOMAXPROCS(0))
	}
	t.Setenv("CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE", "0")
	t.Setenv("CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS", "3")
	iam = DefaultConfig().IAM
	if iam.TokenRequestsPerMinute != 0 || iam.TokenMaxConcurrentSecretChecks != 3 {
		t.Errorf("env override: requests/min %d, secret checks %d, want 0 and 3", iam.TokenRequestsPerMinute, iam.TokenMaxConcurrentSecretChecks)
	}
	if err := ValidateIAM(iam); err != nil {
		t.Errorf("0 requests/min (unlimited) and 3 secret checks refused: %v", err)
	}
}

// A negative per-client limit and fewer than one concurrent secret check
// each refuse to start, in either mode.
func TestValidateIAM_TokenEndpointCost(t *testing.T) {
	for _, mode := range []string{"mock", "jwt"} {
		for k, v := range map[string]string{
			"CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE":          "-1",
			"CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS": "0",
		} {
			t.Run(mode+"/"+k, func(t *testing.T) {
				t.Setenv("CYODA_IAM_MODE", mode)
				t.Setenv(k, v)
				if err := ValidateIAM(DefaultConfig().IAM); err == nil {
					t.Fatalf("%s=%s accepted", k, v)
				}
			})
		}
	}
}

func TestDefaultConfig_HTTPTimeouts(t *testing.T) {
	cfg := DefaultConfig()
	want := HTTPConfig{ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	if cfg.HTTP != want {
		t.Fatalf("HTTP defaults = %+v, want %+v", cfg.HTTP, want)
	}
	t.Setenv("CYODA_HTTP_READ_HEADER_TIMEOUT", "3s")
	t.Setenv("CYODA_HTTP_READ_TIMEOUT", "1m")
	t.Setenv("CYODA_HTTP_WRITE_TIMEOUT", "45s")
	t.Setenv("CYODA_HTTP_IDLE_TIMEOUT", "30s")
	cfg = DefaultConfig()
	want = HTTPConfig{ReadHeaderTimeout: 3 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 45 * time.Second, IdleTimeout: 30 * time.Second}
	if cfg.HTTP != want {
		t.Fatalf("HTTP from env = %+v, want %+v", cfg.HTTP, want)
	}
}

func TestConfig_SearchJobMaxAttempts_Default(t *testing.T) {
	os.Unsetenv("CYODA_SEARCH_JOB_MAX_ATTEMPTS")
	if got := DefaultConfig().SearchJobMaxAttempts; got != 3 {
		t.Fatalf("default = %d, want 3", got)
	}
}
func TestConfig_SearchJobMaxAttempts_Override(t *testing.T) {
	t.Setenv("CYODA_SEARCH_JOB_MAX_ATTEMPTS", "5")
	if got := DefaultConfig().SearchJobMaxAttempts; got != 5 {
		t.Fatalf("override = %d, want 5", got)
	}
}
