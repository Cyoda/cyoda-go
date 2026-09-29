package app

import (
	"os"
	"testing"
)

func TestDefaultConfig_M2MAdminRoleEnabled_DefaultFalse(t *testing.T) {
	t.Setenv("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED", "")
	_ = os.Unsetenv("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED")
	cfg := DefaultConfig()
	if cfg.IAM.M2MAdminRoleEnabled {
		t.Fatalf("expected CYODA_IAM_M2M_ADMIN_ROLE_ENABLED to default to false, got true")
	}
}

func TestDefaultConfig_M2MAdminRoleEnabled_EnvTrue(t *testing.T) {
	t.Setenv("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED", "true")
	cfg := DefaultConfig()
	if !cfg.IAM.M2MAdminRoleEnabled {
		t.Fatalf("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true should set the field to true")
	}
}

func TestDefaultConfig_M2MClientMaxPerTenant(t *testing.T) {
	t.Setenv("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT", "")
	_ = os.Unsetenv("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT")
	if got := DefaultConfig().IAM.M2MClientMaxPerTenant; got != 100 {
		t.Fatalf("default M2MClientMaxPerTenant = %d, want 100", got)
	}
	t.Setenv("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT", "0")
	if got := DefaultConfig().IAM.M2MClientMaxPerTenant; got != 0 {
		t.Fatalf("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT=0: got %d", got)
	}
}

func TestAuthIAMFeatures_PropagatesM2MClientMaxPerTenant(t *testing.T) {
	if got := (IAMConfig{M2MClientMaxPerTenant: 7}).AuthIAMFeatures().M2MClientMaxPerTenant; got != 7 {
		t.Fatalf("AuthIAMFeatures must propagate M2MClientMaxPerTenant, got %d", got)
	}
}

// A negative cap refuses to start.
func TestValidateIAM_RefusesNegativeM2MClientMax(t *testing.T) {
	t.Setenv("CYODA_IAM_MODE", "jwt")
	t.Setenv("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT", "-1")
	if err := ValidateIAM(DefaultConfig().IAM); err == nil {
		t.Fatal("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT=-1 accepted")
	}
}

func TestAuthIAMFeatures_PropagatesM2MAdminRoleEnabled(t *testing.T) {
	c := IAMConfig{M2MAdminRoleEnabled: true}
	f := c.AuthIAMFeatures()
	if !f.M2MAdminRoleEnabled {
		t.Fatalf("AuthIAMFeatures must propagate M2MAdminRoleEnabled, got false")
	}
}
