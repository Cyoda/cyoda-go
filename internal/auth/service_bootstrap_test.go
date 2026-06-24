package auth_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestBootstrapKey_DefaultValiditySet verifies that the bootstrap signing key
// is saved with the configured audience and a ValidTo derived from
// IAMFeatures.KeypairDefaultValidityDays.
func TestBootstrapKey_DefaultValiditySet(t *testing.T) {
	pem := generateTestPEM(t)

	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pem,
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

	kid := svc.SigningKID()
	ks := svc.KeyStore()

	pairs := ks.ListForVerification()

	var found *auth.KeyPair
	for _, kp := range pairs {
		if kp.KID == kid {
			found = kp
			break
		}
	}
	if found == nil {
		t.Fatalf("bootstrap key KID=%q not found in key store", kid)
	}

	// Audience should be set from IAMFeatures.BootstrapAudience.
	if found.Audience != "client" {
		t.Errorf("audience = %q, want %q", found.Audience, "client")
	}

	// ValidTo must be set and approximately 90 days from now.
	if found.ValidTo == nil {
		t.Fatal("bootstrap key ValidTo is nil; expected non-nil")
	}
	wantApprox := found.ValidFrom.Add(90 * 24 * time.Hour)
	diff := found.ValidTo.Sub(wantApprox)
	if diff < -time.Minute || diff > time.Minute {
		t.Errorf("ValidTo = %v, want ~%v (diff %v)", found.ValidTo, wantApprox, diff)
	}
}

// TestBootstrapKey_WarnOnNearExpiry verifies that NewAuthService emits a
// slog.Warn when the configured KeypairDefaultValidityDays is below 30 days.
func TestBootstrapKey_WarnOnNearExpiry(t *testing.T) {
	pem := generateTestPEM(t)

	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	old := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(old) })

	_, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pem,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures: auth.IAMFeatures{
			KeypairDefaultValidityDays: 1, // <30 days — should WARN
			BootstrapAudience:          "client",
			TrustedKeyMaxPerTenant:     10,
			TrustedKeyMaxValidityDays:  365,
			TrustedKeyMaxJWKProperties: 20,
		},
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "bootstrap signing key expires within 30 days") {
		t.Errorf("expected WARN about near-expiry bootstrap key; got log:\n%s", logged)
	}
}

// TestBootstrapKey_NoWarnOnFarExpiry verifies no WARN is emitted when the key
// validity is well beyond 30 days.
func TestBootstrapKey_NoWarnOnFarExpiry(t *testing.T) {
	pem := generateTestPEM(t)

	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	old := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(old) })

	_, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pem,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures: auth.IAMFeatures{
			KeypairDefaultValidityDays: 365, // well beyond 30 days
			BootstrapAudience:          "client",
			TrustedKeyMaxPerTenant:     10,
			TrustedKeyMaxValidityDays:  365,
			TrustedKeyMaxJWKProperties: 20,
		},
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, "bootstrap signing key expires within 30 days") {
		t.Errorf("unexpected WARN for far-expiry bootstrap key; got log:\n%s", logged)
	}
}

// TestBootstrapKey_DefaultIAMFeaturesApplied verifies that when IAMFeatures
// is zero-value, defaults are applied so KeypairDefaultValidityDays > 0
// and the key gets a non-nil ValidTo.
func TestBootstrapKey_DefaultIAMFeaturesApplied(t *testing.T) {
	pem := generateTestPEM(t)

	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pem,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		// IAMFeatures deliberately omitted — should use DefaultIAMFeatures().
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	pairs := svc.KeyStore().ListForVerification()
	if len(pairs) == 0 {
		t.Fatal("expected at least one key pair")
	}
	if pairs[0].ValidTo == nil {
		t.Error("ValidTo is nil when IAMFeatures is zero-value; expected default to apply")
	}
}
