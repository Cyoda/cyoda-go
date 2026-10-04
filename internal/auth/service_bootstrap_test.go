package auth_test

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestBootstrapKey_HasNoWindow: the bootstrap signing key comes from
// CYODA_JWT_SIGNING_KEY and lives as long as that configuration. It has no
// ValidTo, and no ValidFrom that a node's clock could ever be before — a
// window counted from each node's start would differ per node, reset on every
// restart, and stop a long-running node from verifying the cluster's tokens.
func TestBootstrapKey_HasNoWindow(t *testing.T) {
	pem := generateTestPEM(t)
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: pem,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures: auth.IAMFeatures{
			KeypairDefaultValidityDays: 90,
			TrustedKeyMaxPerTenant:     10,
			TrustedKeyMaxValidityDays:  365,
			TrustedKeyMaxJWKProperties: 20,
		},
	})

	kp, err := svc.KeyStore().Current()
	if err != nil {
		t.Fatalf("bootstrap key does not sign: %v", err)
	}
	if kp.KID != pemKID(t, pem) || !kp.Bootstrap {
		t.Fatalf("signing key = %s (bootstrap %v), want the bootstrap key %s", kp.KID, kp.Bootstrap, pemKID(t, pem))
	}
	if kp.ValidTo != nil {
		t.Errorf("ValidTo = %v, want none", kp.ValidTo)
	}
	if !kp.ValidFrom.IsZero() {
		t.Errorf("ValidFrom = %v, want the zero time (no start bound)", kp.ValidFrom)
	}
}

// TestBootstrapKey_ConfiguredIAMFeaturesKept: only a wholly unset
// IAMFeatures takes the defaults; one that is set is used as given, so a
// field it sets is not overwritten back to the default. Proven here with
// M2MClientMaxPerTenant, a field NewAuthService actually reads (unlike
// KeypairDefaultValidityDays, which only the key-pair issue adapter
// consults): a cap of 1 refuses a tenant's second client.
func TestBootstrapKey_ConfiguredIAMFeaturesKept(t *testing.T) {
	features := auth.DefaultIAMFeatures()
	features.M2MClientMaxPerTenant = 1
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   features,
	})
	store := svc.M2MClientStore()
	if _, err := store.Create(systemCtx(), "tenant-a", "CLIENT1", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatalf("first client: %v", err)
	}
	if _, err := store.Create(systemCtx(), "tenant-a", "CLIENT2", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientCapReached) {
		t.Fatalf("second client with M2MClientMaxPerTenant=1: err = %v, want ErrM2MClientCapReached", err)
	}
}

// TestNewAuthService_RejectsInvalidIAMFeatures: a partly set IAMFeatures is
// validated, not silently used — a zero TrustedKeyMaxValidityDays (among
// other fields that must be > 0) fails startup rather than being silently
// accepted.
func TestNewAuthService_RejectsInvalidIAMFeatures(t *testing.T) {
	_, err := auth.NewAuthService(systemCtx(), auth.AuthConfig{
		KV:            mustNewMemoryKV(t, systemCtx()),
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   auth.IAMFeatures{M2MAdminRoleEnabled: true},
	})
	if err == nil {
		t.Fatal("NewAuthService accepted an invalid partly set IAMFeatures")
	}
}

// TestBootstrapKey_DefaultIAMFeaturesApplied: a zero-value IAMFeatures takes
// the defaults, so the bootstrap key signs.
func TestBootstrapKey_DefaultIAMFeaturesApplied(t *testing.T) {
	pem := generateTestPEM(t)
	svc := newTestAuthService(t, auth.AuthConfig{
		SigningKeyPEM: pem,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		// IAMFeatures deliberately omitted — should use DefaultIAMFeatures().
	})

	kp, err := svc.KeyStore().Current()
	if err != nil {
		t.Fatalf("bootstrap key does not sign: %v", err)
	}
	if kp.KID != pemKID(t, pem) || !kp.Bootstrap {
		t.Fatalf("signing key = %s (bootstrap %v), want the bootstrap key %s", kp.KID, kp.Bootstrap, pemKID(t, pem))
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

// pemKID is the KID of the bootstrap key a PEM configures.
func pemKID(t *testing.T, pem string) string {
	t.Helper()
	k, err := auth.ParseRSAPrivateKeyFromPEM([]byte(pem))
	if err != nil {
		t.Fatal(err)
	}
	kid, err := auth.DeriveKID(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return kid
}
