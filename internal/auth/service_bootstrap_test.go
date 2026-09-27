package auth_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestBootstrapKey_HasNoWindow: the bootstrap signing key comes from
// CYODA_JWT_SIGNING_KEY and lives as long as that configuration. It has no
// ValidTo, and no ValidFrom that a node's clock could ever be before — a
// window counted from each node's start would differ per node, reset on every
// restart, and stop a long-running node from verifying the cluster's tokens.
func TestBootstrapKey_HasNoWindow(t *testing.T) {
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures: auth.IAMFeatures{
			KeypairDefaultValidityDays: 90,
			BootstrapAudience:          "client",
			TrustedKeyMaxPerTenant:     10,
			TrustedKeyMaxValidityDays:  365,
			TrustedKeyMaxJWKProperties: 20,
		},
	})

	kp, err := svc.KeyStore().Current("client")
	if err != nil {
		t.Fatalf("bootstrap key does not sign: %v", err)
	}
	if kp.KID != svc.SigningKID() || !kp.Bootstrap {
		t.Fatalf("signing key = %s (bootstrap %v), want the bootstrap key %s", kp.KID, kp.Bootstrap, svc.SigningKID())
	}
	if kp.Audience != "client" {
		t.Errorf("audience = %q, want client", kp.Audience)
	}
	if kp.ValidTo != nil {
		t.Errorf("ValidTo = %v, want none", kp.ValidTo)
	}
	if !kp.ValidFrom.IsZero() {
		t.Errorf("ValidFrom = %v, want the zero time (no start bound)", kp.ValidFrom)
	}
}

// TestBootstrapKey_ConfiguredIAMFeaturesKept: only a wholly unset
// IAMFeatures takes the defaults; one that is set is used as given, so its
// BootstrapAudience is not overwritten.
func TestBootstrapKey_ConfiguredIAMFeaturesKept(t *testing.T) {
	features := auth.DefaultIAMFeatures()
	features.BootstrapAudience = "human"
	features.KeypairDefaultValidityDays = 30
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   features,
	})
	kp, err := svc.KeyStore().Current("human")
	if err != nil {
		t.Fatalf("bootstrap key does not sign: %v", err)
	}
	if kp.KID != svc.SigningKID() || !kp.Bootstrap {
		t.Fatalf("signing key = %s (bootstrap %v), want the bootstrap key %s", kp.KID, kp.Bootstrap, svc.SigningKID())
	}
	if kp.Audience != "human" {
		t.Errorf("audience = %q, want the configured human", kp.Audience)
	}
}

// TestNewAuthService_RejectsInvalidIAMFeatures: a partly set IAMFeatures is
// validated, not silently used — an empty BootstrapAudience would leave the
// bootstrap key under an audience no token is signed for.
func TestNewAuthService_RejectsInvalidIAMFeatures(t *testing.T) {
	_, err := auth.NewAuthService(systemCtx(), auth.AuthConfig{
		KV:            mustNewMemoryKV(t, systemCtx()),
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   auth.IAMFeatures{M2MAdminRoleEnabled: true},
	})
	if err == nil {
		t.Fatal("NewAuthService accepted an IAMFeatures with no bootstrap audience")
	}
}

// TestBootstrapKey_DefaultIAMFeaturesApplied: a zero-value IAMFeatures takes
// the defaults, so the bootstrap key gets the default audience.
func TestBootstrapKey_DefaultIAMFeaturesApplied(t *testing.T) {
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		// IAMFeatures deliberately omitted — should use DefaultIAMFeatures().
	})

	kp, err := svc.KeyStore().Current(auth.DefaultIAMFeatures().BootstrapAudience)
	if err != nil {
		t.Fatalf("bootstrap key does not sign: %v", err)
	}
	if kp.KID != svc.SigningKID() || !kp.Bootstrap {
		t.Fatalf("signing key = %s (bootstrap %v), want the bootstrap key %s", kp.KID, kp.Bootstrap, svc.SigningKID())
	}
	if want := auth.DefaultIAMFeatures().BootstrapAudience; kp.Audience != want {
		t.Errorf("audience = %q, want the default %q", kp.Audience, want)
	}
}

// TestNewAuthService_RequiresKV: the key stores live in the KV store, so a
// service without one is a wiring error, reported rather than panicking.
func TestNewAuthService_RequiresKV(t *testing.T) {
	_, err := auth.NewAuthService(systemCtx(), auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err == nil {
		t.Fatal("NewAuthService accepted a config with no KV store")
	}
}
