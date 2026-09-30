package app

import (
	"fmt"
	"strings"
	"testing"
)

func TestDefaultConfig_RequireJWT_DefaultsFalse(t *testing.T) {
	t.Setenv("CYODA_REQUIRE_JWT", "")
	cfg := DefaultConfig()
	if cfg.IAM.RequireJWT {
		t.Fatalf("RequireJWT should default to false; got true")
	}
}

func TestValidateIAM_RequireJWTFalse_AllowsMock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.RequireJWT = false
	cfg.IAM.Mode = "mock"
	if err := ValidateIAM(cfg.IAM); err != nil {
		t.Fatalf("expected nil; got %v", err)
	}
}

func TestValidateIAM_RequireJWTTrue_AllowsJWTWithKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.RequireJWT = true
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----"
	if err := ValidateIAM(cfg.IAM); err != nil {
		t.Fatalf("expected nil; got %v", err)
	}
}

func TestValidateIAM_RequireJWTTrue_RejectsMockMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.RequireJWT = true
	cfg.IAM.Mode = "mock"
	err := ValidateIAM(cfg.IAM)
	if err == nil {
		t.Fatal("expected error for mock mode with RequireJWT=true")
	}
	if !strings.Contains(err.Error(), "CYODA_IAM_MODE") {
		t.Fatalf("error should name the offending env var; got %v", err)
	}
}

func TestValidateIAM_RequireJWTTrue_RejectsMissingKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.RequireJWT = true
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = ""
	err := ValidateIAM(cfg.IAM)
	if err == nil {
		t.Fatal("expected error for missing signing key with RequireJWT=true")
	}
	if !strings.Contains(err.Error(), "CYODA_JWT_SIGNING_KEY") {
		t.Fatalf("error should name the offending env var; got %v", err)
	}
}

func TestValidateIAM_RejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"JWT", "Mock", "", "none", "jwt "} {
		t.Run(fmt.Sprintf("%q", mode), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.IAM.RequireJWT = false
			cfg.IAM.Mode = mode
			err := ValidateIAM(cfg.IAM)
			if err == nil {
				t.Fatalf("mode %q: expected an error, got nil", mode)
			}
			if !strings.Contains(err.Error(), "CYODA_IAM_MODE") {
				t.Errorf("error should name CYODA_IAM_MODE: %v", err)
			}
		})
	}
}

func TestValidateIAM_AcceptsJWTWithoutRequireJWT(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.RequireJWT = false
	cfg.IAM.Mode = "jwt"
	if err := ValidateIAM(cfg.IAM); err != nil {
		t.Fatalf("expected nil; got %v", err)
	}
}
