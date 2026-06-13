package auth_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestIAMFeatures_Validate_Defaults(t *testing.T) {
	if err := auth.DefaultIAMFeatures().Validate(); err != nil {
		t.Fatalf("default should validate, got %v", err)
	}
}

func TestIAMFeatures_Validate_Rejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*auth.IAMFeatures)
	}{
		{"BootstrapAudience invalid", func(c *auth.IAMFeatures) { c.BootstrapAudience = "robot" }},
		{"BootstrapAudience empty", func(c *auth.IAMFeatures) { c.BootstrapAudience = "" }},
		{"TrustedKeyMaxPerTenant negative", func(c *auth.IAMFeatures) { c.TrustedKeyMaxPerTenant = -1 }},
		{"TrustedKeyMaxValidityDays zero", func(c *auth.IAMFeatures) { c.TrustedKeyMaxValidityDays = 0 }},
		{"TrustedKeyMaxJWKProperties zero", func(c *auth.IAMFeatures) { c.TrustedKeyMaxJWKProperties = 0 }},
		{"KeypairDefaultValidityDays zero", func(c *auth.IAMFeatures) { c.KeypairDefaultValidityDays = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := auth.DefaultIAMFeatures()
			tc.mut(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestIAMFeatures_Validate_MaxPerTenantZeroIsUnbounded(t *testing.T) {
	c := auth.DefaultIAMFeatures()
	c.TrustedKeyMaxPerTenant = 0
	if err := c.Validate(); err != nil {
		t.Errorf("MaxPerTenant=0 means unbounded; got %v", err)
	}
}
