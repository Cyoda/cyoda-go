package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Metrics-bearer env resolution: follows the same _FILE suffix convention
// as the other credential env vars (CYODA_POSTGRES_URL, CYODA_JWT_SIGNING_KEY,
// CYODA_HMAC_SECRET).

func TestConfig_MetricsBearer_PlainEnv(t *testing.T) {
	t.Setenv("CYODA_METRICS_BEARER", "plain-bearer")
	t.Setenv("CYODA_METRICS_BEARER_FILE", "")
	cfg := DefaultConfig()
	if cfg.Admin.MetricsBearerToken != "plain-bearer" {
		t.Errorf("plain env: got %q, want %q", cfg.Admin.MetricsBearerToken, "plain-bearer")
	}
}

func TestConfig_MetricsBearer_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bearer")
	if err := os.WriteFile(path, []byte("file-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYODA_METRICS_BEARER", "")
	t.Setenv("CYODA_METRICS_BEARER_FILE", path)
	cfg := DefaultConfig()
	if cfg.Admin.MetricsBearerToken != "file-bearer" {
		t.Errorf("_FILE: got %q, want %q (trailing whitespace must be trimmed)", cfg.Admin.MetricsBearerToken, "file-bearer")
	}
}

func TestConfig_MetricsBearer_FileWinsOverPlain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bearer")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYODA_METRICS_BEARER", "from-env")
	t.Setenv("CYODA_METRICS_BEARER_FILE", path)
	cfg := DefaultConfig()
	if cfg.Admin.MetricsBearerToken != "from-file" {
		t.Errorf("_FILE must win over plain env: got %q", cfg.Admin.MetricsBearerToken)
	}
}

func TestConfig_MetricsRequireAuth_Default(t *testing.T) {
	t.Setenv("CYODA_METRICS_REQUIRE_AUTH", "")
	cfg := DefaultConfig()
	if cfg.Admin.MetricsRequireAuth {
		t.Error("default must be false (desktop/docker unaffected); got true")
	}
}

func TestConfig_MetricsRequireAuth_EnvTrue(t *testing.T) {
	t.Setenv("CYODA_METRICS_REQUIRE_AUTH", "true")
	cfg := DefaultConfig()
	if !cfg.Admin.MetricsRequireAuth {
		t.Error("CYODA_METRICS_REQUIRE_AUTH=true must set config; got false")
	}
}

// validateMetricsAuth: coupled predicate.
//   - RequireAuth=true, token unset  → fatal startup error.
//   - RequireAuth=true, token set    → OK.
//   - RequireAuth=false, token set   → OK (auth applied anyway — if the
//     operator configured a token, they want it enforced).
//   - RequireAuth=false, token unset → OK (no auth).

func TestValidateMetricsAuth_RequiredButEmpty_Rejects(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Admin.MetricsRequireAuth = true
	cfg.Admin.MetricsBearerToken = ""
	if err := validateMetricsAuth(&cfg); err == nil {
		t.Fatal("expected error when CYODA_METRICS_REQUIRE_AUTH=true but bearer unset; got nil")
	} else if !strings.Contains(err.Error(), "CYODA_METRICS_BEARER") {
		t.Errorf("error must name the missing env var; got: %v", err)
	}
}

func TestValidateMetricsAuth_RequiredAndSet_OK(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Admin.MetricsRequireAuth = true
	cfg.Admin.MetricsBearerToken = "some-token"
	if err := validateMetricsAuth(&cfg); err != nil {
		t.Errorf("required + set should be valid; got: %v", err)
	}
}

func TestValidateMetricsAuth_NotRequiredSetOrUnset_OK(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Admin.MetricsRequireAuth = false
	cfg.Admin.MetricsBearerToken = ""
	if err := validateMetricsAuth(&cfg); err != nil {
		t.Errorf("not required, no token: should be valid; got: %v", err)
	}
	cfg.Admin.MetricsBearerToken = "opt-in-token"
	if err := validateMetricsAuth(&cfg); err != nil {
		t.Errorf("not required but token set: still valid (token drives auth); got: %v", err)
	}
}
