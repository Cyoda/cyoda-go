package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestBootstrapSecret_JwtMode_IDSetSecretUnset_Rejects verifies that in jwt mode,
// setting CYODA_BOOTSTRAP_CLIENT_ID without CYODA_BOOTSTRAP_CLIENT_SECRET is rejected.
func TestBootstrapSecret_JwtMode_IDSetSecretUnset_Rejects(t *testing.T) {
	cfg := bootstrapTestConfig("jwt", "client-id", "")
	_, err := validateBootstrapConfig(cfg)
	if err == nil {
		t.Fatal("expected error when CYODA_BOOTSTRAP_CLIENT_ID set but CYODA_BOOTSTRAP_CLIENT_SECRET unset in jwt mode; got nil")
	}
	if !strings.Contains(err.Error(), "CYODA_BOOTSTRAP_CLIENT_SECRET") {
		t.Errorf("error should name the missing env var; got: %v", err)
	}
}

// TestBootstrapSecret_JwtMode_SecretSetIDUnset_Rejects verifies that in jwt mode,
// setting CYODA_BOOTSTRAP_CLIENT_SECRET without CYODA_BOOTSTRAP_CLIENT_ID is rejected.
func TestBootstrapSecret_JwtMode_SecretSetIDUnset_Rejects(t *testing.T) {
	cfg := bootstrapTestConfig("jwt", "", "some-secret")
	_, err := validateBootstrapConfig(cfg)
	if err == nil {
		t.Fatal("expected error when CYODA_BOOTSTRAP_CLIENT_SECRET set but CYODA_BOOTSTRAP_CLIENT_ID unset in jwt mode; got nil")
	}
	if !strings.Contains(err.Error(), "CYODA_BOOTSTRAP_CLIENT_ID") {
		t.Errorf("error should name the missing env var; got: %v", err)
	}
}

// TestBootstrapSecret_JwtMode_BothEmpty_OK verifies that jwt mode with neither
// ID nor secret set is a legitimate startup (JWKS-only auth, no bootstrap M2M client).
func TestBootstrapSecret_JwtMode_BothEmpty_OK(t *testing.T) {
	cfg := bootstrapTestConfig("jwt", "", "")
	_, err := validateBootstrapConfig(cfg)
	if err != nil {
		t.Errorf("jwt mode with neither ID nor secret should be valid (no bootstrap M2M client); got: %v", err)
	}
}

// TestBootstrapSecret_MockModeIgnored verifies that mock mode doesn't
// require the bootstrap secret.
func TestBootstrapSecret_MockModeIgnored(t *testing.T) {
	cfg := bootstrapTestConfig("mock", "", "")
	_, err := validateBootstrapConfig(cfg)
	if err != nil {
		t.Errorf("mock mode should not require bootstrap secret; got: %v", err)
	}
}

// TestBootstrapSecret_NotLogged verifies that no secret value is
// ever written to the slog default handler.
func TestBootstrapSecret_NotLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "canary-secret-value-must-not-appear-in-logs"
	cfg := bootstrapTestConfig("jwt", "some-client-id", secret)

	if _, err := validateBootstrapConfig(cfg); err != nil {
		t.Fatalf("validate failed: %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("bootstrap client secret MUST NOT appear in logs; output:\n%s", buf.String())
	}
}

// bootstrapTestConfig builds a minimal Config for bootstrap validation tests.
func bootstrapTestConfig(iamMode, clientID, clientSecret string) *Config {
	cfg := DefaultConfig()
	cfg.IAM.Mode = iamMode
	cfg.Bootstrap.ClientID = clientID
	cfg.Bootstrap.ClientSecret = clientSecret
	return &cfg
}

// TestValidateBootstrapConfig_TenantGrammar pins door 2. A bootstrap tenant is
// operator-supplied configuration, so it is validated at startup and the
// process refuses to come up rather than running with a tenant that cannot be
// addressed consistently.
func TestValidateBootstrapConfig_TenantGrammar(t *testing.T) {
	base := func(tenant string) *Config {
		cfg := DefaultConfig()
		cfg.IAM.Mode = "jwt"
		cfg.Bootstrap.ClientID = "bootstrap-client"
		cfg.Bootstrap.ClientSecret = "bootstrap-secret"
		cfg.Bootstrap.TenantID = tenant
		return &cfg
	}

	for name, tenant := range map[string]string{
		"traversal": "../victim",
		"slash":     "a/b",
		"empty":     "",
		"too-long":  strings.Repeat("x", 101),
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			if _, err := validateBootstrapConfig(base(tenant)); err == nil {
				t.Fatalf("validateBootstrapConfig accepted tenant %q", tenant)
			}
		})
	}

	for _, tenant := range []string{"default-tenant", "SYSTEM", "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"} {
		t.Run("accept/"+tenant, func(t *testing.T) {
			if _, err := validateBootstrapConfig(base(tenant)); err != nil {
				t.Fatalf("validateBootstrapConfig(%q) = %v, want nil", tenant, err)
			}
		})
	}
}

// TestValidateBootstrapConfig_EmptyTenantWithoutBootstrapClient guards the one
// deployment shape the check could otherwise break. envString uses LookupEnv,
// so an explicitly-empty CYODA_BOOTSTRAP_TENANT_ID overrides the default — and
// a deployment that configures no bootstrap client never consumes the tenant
// at all, so it must still start.
func TestValidateBootstrapConfig_EmptyTenantWithoutBootstrapClient(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.Mode = "jwt"
	cfg.Bootstrap.ClientID = ""
	cfg.Bootstrap.ClientSecret = ""
	cfg.Bootstrap.TenantID = ""

	if _, err := validateBootstrapConfig(&cfg); err != nil {
		t.Fatalf("validateBootstrapConfig = %v, want nil when no bootstrap client is configured", err)
	}
}

// TestShippedTenantConstantsSatisfyGrammar stops a later change to a default
// from producing a binary that cannot start, or a mock mode that cannot
// authenticate.
func TestShippedTenantConstantsSatisfyGrammar(t *testing.T) {
	cfg := DefaultConfig()
	for name, id := range map[string]spi.TenantID{
		"spi.SystemTenantID":            spi.SystemTenantID,
		"IAM.MockTenantID":              spi.TenantID(cfg.IAM.MockTenantID),
		"CYODA_BOOTSTRAP_TENANT_ID dflt": spi.TenantID(cfg.Bootstrap.TenantID),
	} {
		if err := common.ValidateTenantID(id); err != nil {
			t.Errorf("%s (%q) fails the tenant grammar: %v", name, id, err)
		}
	}
}
