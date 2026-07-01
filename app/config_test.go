package app

import (
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
