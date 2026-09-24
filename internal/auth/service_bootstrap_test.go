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
	svc, err := auth.NewAuthService(auth.AuthConfig{
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
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	kp, err := svc.KeyStore().Get(svc.SigningKID())
	if err != nil {
		t.Fatalf("bootstrap key not in the key store: %v", err)
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

// TestBootstrapKey_PartialIAMFeaturesKept: only a wholly unset IAMFeatures
// takes the defaults; one with any field set is used as given, so its
// BootstrapAudience is not overwritten.
func TestBootstrapKey_PartialIAMFeaturesKept(t *testing.T) {
	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   auth.IAMFeatures{BootstrapAudience: "human"},
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	kp, err := svc.KeyStore().Get(svc.SigningKID())
	if err != nil {
		t.Fatalf("bootstrap key not in the key store: %v", err)
	}
	if kp.Audience != "human" {
		t.Errorf("audience = %q, want the configured human", kp.Audience)
	}
}

// TestBootstrapKey_DefaultIAMFeaturesApplied: a zero-value IAMFeatures takes
// the defaults, so the bootstrap key gets the default audience.
func TestBootstrapKey_DefaultIAMFeaturesApplied(t *testing.T) {
	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		// IAMFeatures deliberately omitted — should use DefaultIAMFeatures().
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	kp, err := svc.KeyStore().Get(svc.SigningKID())
	if err != nil {
		t.Fatalf("bootstrap key not in the key store: %v", err)
	}
	if want := auth.DefaultIAMFeatures().BootstrapAudience; kp.Audience != want {
		t.Errorf("audience = %q, want the default %q", kp.Audience, want)
	}
}
