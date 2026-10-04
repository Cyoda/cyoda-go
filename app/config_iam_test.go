package app

import (
	"bytes"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Mock IAM defaults must grant ROLE_M2M so the gRPC streaming service accepts
// connections from a default mock-authenticated client, and ROLE_ADMIN so the
// admin HTTP endpoints accept the same user.
func TestDefaultConfig_MockRolesIncludeM2MAndAdmin(t *testing.T) {
	// t.Setenv registers cleanup, then Unsetenv ensures the var is absent
	// so we observe the DefaultConfig fallback, not an inherited value.
	t.Setenv("CYODA_IAM_MOCK_ROLES", "")
	_ = os.Unsetenv("CYODA_IAM_MOCK_ROLES")
	cfg := DefaultConfig()

	want := []string{"ROLE_ADMIN", "ROLE_M2M"}
	if !reflect.DeepEqual(cfg.IAM.MockRoles, want) {
		t.Errorf("expected default MockRoles=%v, got %v", want, cfg.IAM.MockRoles)
	}
}

func TestDefaultConfig_MockRolesOverrideFromEnv(t *testing.T) {
	t.Setenv("CYODA_IAM_MOCK_ROLES", "ROLE_M2M,ROLE_USER")
	cfg := DefaultConfig()

	want := []string{"ROLE_M2M", "ROLE_USER"}
	if !reflect.DeepEqual(cfg.IAM.MockRoles, want) {
		t.Errorf("expected MockRoles=%v, got %v", want, cfg.IAM.MockRoles)
	}
}

func TestDefaultConfig_MockRolesOverrideTrimsWhitespace(t *testing.T) {
	t.Setenv("CYODA_IAM_MOCK_ROLES", " ROLE_M2M , ROLE_ADMIN ")
	cfg := DefaultConfig()

	want := []string{"ROLE_M2M", "ROLE_ADMIN"}
	if !reflect.DeepEqual(cfg.IAM.MockRoles, want) {
		t.Errorf("expected MockRoles=%v, got %v", want, cfg.IAM.MockRoles)
	}
}

func TestDefaultConfig_MockRolesOverrideEmptyStringFallsBackToDefault(t *testing.T) {
	t.Setenv("CYODA_IAM_MOCK_ROLES", "")
	cfg := DefaultConfig()

	want := []string{"ROLE_ADMIN", "ROLE_M2M"}
	if !reflect.DeepEqual(cfg.IAM.MockRoles, want) {
		t.Errorf("expected default MockRoles when env empty, got %v", cfg.IAM.MockRoles)
	}
}

// When CYODA_IAM_MOCK_ROLES is set but resolves to no entries
// (empty or only-whitespace/commas), the silent fallback to the full-admin
// default is dangerous — the operator clearly *tried* to lock the mock user
// down. Warn loudly so the misconfiguration isn't missed.
func TestDefaultConfig_MockRolesEmptyOverrideWarns(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"empty string", ""},
		{"all whitespace", "   "},
		{"only commas", ",,"},
		{"whitespace and commas", " , ,  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			t.Setenv("CYODA_IAM_MOCK_ROLES", tc.value)
			_ = DefaultConfig()

			out := buf.String()
			if !strings.Contains(out, "CYODA_IAM_MOCK_ROLES") {
				t.Errorf("expected WARN mentioning env var, got:\n%s", out)
			}
			if !strings.Contains(out, "level=WARN") {
				t.Errorf("expected WARN level log, got:\n%s", out)
			}
		})
	}
}

func TestDefaultConfig_MockRolesValidOverrideDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("CYODA_IAM_MOCK_ROLES", "ROLE_USER")
	_ = DefaultConfig()

	if strings.Contains(buf.String(), "CYODA_IAM_MOCK_ROLES") {
		t.Errorf("unexpected log output for valid override:\n%s", buf.String())
	}
}

// TestShippedUserIDConstantsPassCheck stops a later change to a default from
// producing a binary that cannot start, or a mock mode whose user fails the
// check every door applies. The platform system principal is the opposite
// case: it must BE the reserved id, built directly rather than through
// ValidateUserID, so it never passes the check itself.
func TestShippedUserIDConstantsPassCheck(t *testing.T) {
	cfg := DefaultConfig()
	for name, id := range map[string]string{
		"IAM.MockUserID": cfg.IAM.MockUserID,
	} {
		if err := common.ValidateUserID(id); err != nil {
			t.Errorf("%s (%q) fails the user-id check: %v", name, id, err)
		}
	}

	systemID := common.SystemPrincipal().ID
	if systemID != common.ReservedSystemUserID {
		t.Errorf("SystemPrincipal().ID = %q, want %q", systemID, common.ReservedSystemUserID)
	}
	if err := common.ValidateUserID(systemID); err == nil {
		t.Errorf("ValidateUserID(%q) = nil, want the system id to be refused as reserved", systemID)
	}
}

// TestShippedTenantConstantsSatisfyGrammar stops a later change to a default
// from producing a binary that cannot start, or a mock mode that cannot
// authenticate.
func TestShippedTenantConstantsSatisfyGrammar(t *testing.T) {
	cfg := DefaultConfig()
	for name, id := range map[string]spi.TenantID{
		"spi.SystemTenantID": spi.SystemTenantID,
		"IAM.MockTenantID":   spi.TenantID(cfg.IAM.MockTenantID),
	} {
		if err := common.ValidateTenantID(id); err != nil {
			t.Errorf("%s (%q) fails the tenant grammar: %v", name, id, err)
		}
	}
}

func TestDefaultConfig_MockRolesUnsetDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("CYODA_IAM_MOCK_ROLES", "")
	_ = os.Unsetenv("CYODA_IAM_MOCK_ROLES")
	_ = DefaultConfig()

	if strings.Contains(buf.String(), "CYODA_IAM_MOCK_ROLES") {
		t.Errorf("unexpected log output when env var is unset:\n%s", buf.String())
	}
}
