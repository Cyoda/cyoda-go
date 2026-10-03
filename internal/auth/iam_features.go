package auth

import "fmt"

// IAMFeatures bundles IAM-feature configuration consumed by the
// /oauth/keys/* and /clients adapter surfaces, the M2M client store and the
// bootstrap-key wiring.
// Named "Features" (not "Config") to avoid name collision with
// app.IAMConfig (app/config.go) which carries IAM mode + JWT settings.
type IAMFeatures struct {
	TrustedKeyRegistrationEnabled bool // env CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED, default false
	TrustedKeyMaxPerTenant        int  // env CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT, default 10; 0=unbounded
	TrustedKeyMaxValidityDays     int  // env CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS, default 365
	TrustedKeyMaxJWKProperties    int  // env CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES, default 20
	KeypairDefaultValidityDays    int  // env CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS, default 365

	// M2MAdminRoleEnabled gates POST /clients?withAdminRole=true. When false
	// (the secure default) the endpoint returns 404 FEATURE_DISABLED on that
	// request shape. env CYODA_IAM_M2M_ADMIN_ROLE_ENABLED, default false.
	M2MAdminRoleEnabled bool

	// M2MClientMaxPerTenant caps the M2M clients one tenant may hold.
	// env CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT, default 100; 0=unbounded.
	M2MClientMaxPerTenant int
}

func DefaultIAMFeatures() IAMFeatures {
	return IAMFeatures{
		TrustedKeyMaxPerTenant:     10,
		TrustedKeyMaxValidityDays:  365,
		TrustedKeyMaxJWKProperties: 20,
		KeypairDefaultValidityDays: 365,
		M2MClientMaxPerTenant:      100,
	}
}

func (c IAMFeatures) Validate() error {
	if c.TrustedKeyMaxPerTenant < 0 {
		return fmt.Errorf("CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT must be >= 0, got %d", c.TrustedKeyMaxPerTenant)
	}
	if c.M2MClientMaxPerTenant < 0 {
		return fmt.Errorf("CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT must be >= 0, got %d", c.M2MClientMaxPerTenant)
	}
	if c.TrustedKeyMaxValidityDays <= 0 {
		return fmt.Errorf("CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS must be > 0, got %d", c.TrustedKeyMaxValidityDays)
	}
	if c.TrustedKeyMaxJWKProperties <= 0 {
		return fmt.Errorf("CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES must be > 0, got %d", c.TrustedKeyMaxJWKProperties)
	}
	if c.KeypairDefaultValidityDays <= 0 {
		return fmt.Errorf("CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS must be > 0, got %d", c.KeypairDefaultValidityDays)
	}
	return nil
}
