package common

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestValidateTenantID_Accepts(t *testing.T) {
	// Every shape that exists in cyoda-go or in Cyoda Cloud today. A failure
	// here is a production or test-fixture lockout, not a style question.
	accepted := []string{
		"SYSTEM",         // spi.SystemTenantID
		"CYODA",          // Cloud's local-issuer fallback
		"default-tenant", // CYODA_BOOTSTRAP_TENANT_ID default
		"mock-tenant",    // IAM mock mode
		"system-tenant",  // parity fixtures
		"riskblocs",      // scripts/multi-node-docker
		"my-tenant",      // scripts README
		"tenant-abc-123", // published OpenAPI example
		"tenant-A",       // case-varied package fixtures
		"tenant-a",
		"123",                                  // Cloud uses bare numerics
		"caas_mock-oidc-org-test-subject",      // Cloud's caas_<org_id> form
		"TEST_LEGAL_ENTITY",                    // Cloud fixtures
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",     // Cloud's generated 32-hex id
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d", // canonical UUID
		"conformance-1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d", // spitest, 48 chars
		"a",                      // shortest legal
		strings.Repeat("x", 100), // longest legal
		"a.b",                    // dot is admitted
	}
	for _, id := range accepted {
		if err := ValidateTenantID(spi.TenantID(id)); err != nil {
			t.Errorf("ValidateTenantID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidateTenantID_Rejects(t *testing.T) {
	rejected := map[string]string{
		"empty":         "",
		"dot":           ".",
		"dotdot":        "..",
		"traversal":     "../victim",
		"hidden":        ".hidden",
		"leading-dash":  "-flag",
		"leading-under": "_x",
		"slash":         "a/b",
		"backslash":     `a\b`,
		"colon":         "a:b",
		"space":         "a b",
		"newline":       "a\nb",
		"cr":            "a\rb",
		"nul":           "a\x00b",
		"tab":           "a\tb",
		"at":            "a@b",
		"percent":       "a%b",
		"quote":         `a"b`,
		"brace":         "{a}",
		"non-ascii":     "tenÅnt",
		"too-long":      strings.Repeat("x", 101),
	}
	for name, id := range rejected {
		t.Run(name, func(t *testing.T) {
			err := ValidateTenantID(spi.TenantID(id))
			if err == nil {
				t.Fatalf("ValidateTenantID(%q) = nil, want an error", id)
			}
			if !errors.Is(err, ErrInvalidTenantID) {
				t.Errorf("error does not wrap ErrInvalidTenantID: %v", err)
			}
		})
	}
}

// TestValidateTenantID_ErrorNeverEchoesValue is the log-injection guard: the
// rejected id reaches slog through the auth failure path's detail field, so an
// attacker-chosen claim must not be able to ride into a log record.
func TestValidateTenantID_ErrorNeverEchoesValue(t *testing.T) {
	needle := "NEEDLE-a/b\nforged-log-line"
	err := ValidateTenantID(spi.TenantID(needle))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "NEEDLE") {
		t.Fatalf("error echoes the rejected value: %q", err.Error())
	}
	if strings.ContainsAny(err.Error(), "\n\r") {
		t.Fatalf("error carries a control character: %q", err.Error())
	}
}
