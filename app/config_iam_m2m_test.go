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

func TestAuthIAMFeatures_PropagatesM2MAdminRoleEnabled(t *testing.T) {
	c := IAMConfig{M2MAdminRoleEnabled: true}
	f := c.AuthIAMFeatures()
	if !f.M2MAdminRoleEnabled {
		t.Fatalf("AuthIAMFeatures must propagate M2MAdminRoleEnabled, got false")
	}
}
