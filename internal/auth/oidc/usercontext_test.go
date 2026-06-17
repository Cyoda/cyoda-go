package oidc

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
)

func provider(rolesClaim *string) *OidcProvider {
	return &OidcProvider{
		ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		OwnerLegalEntityID: uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"),
		RolesClaim:         rolesClaim,
	}
}

func TestBuildOIDCUserContext_NamespacedUserID(t *testing.T) {
	p := provider(nil)
	uc, err := buildOIDCUserContext(p, map[string]any{"sub": "alice"}, "roles")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expected := "oidc:11111111-2222-3333-4444-555555555555:alice"
	if uc.UserID != expected {
		t.Errorf("UserID = %q, want %q", uc.UserID, expected)
	}
	if uc.Tenant.ID != "66666666-7777-8888-9999-aaaaaaaaaaaa" {
		t.Errorf("Tenant.ID = %q, want %q", uc.Tenant.ID, "66666666-7777-8888-9999-aaaaaaaaaaaa")
	}
	if uc.UserName != "alice" {
		t.Errorf("UserName = %q, want alice", uc.UserName)
	}
}

func TestBuildOIDCUserContext_RolesFromGlobalDefault(t *testing.T) {
	p := provider(nil)
	uc, err := buildOIDCUserContext(p, map[string]any{
		"sub": "alice", "roles": []any{"admin", "user"},
	}, "roles")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(uc.Roles) != 2 {
		t.Errorf("Roles = %v, want 2 elements", uc.Roles)
	}
}

func TestBuildOIDCUserContext_PerProviderRolesClaim(t *testing.T) {
	rc := "cognito:groups"
	p := provider(&rc)
	uc, err := buildOIDCUserContext(p, map[string]any{
		"sub": "bob", "cognito:groups": []any{"admins"}, "roles": []any{"ignored"},
	}, "roles")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(uc.Roles) != 1 || uc.Roles[0] != "admins" {
		t.Errorf("Roles = %v, want [admins]", uc.Roles)
	}
}

func TestBuildOIDCUserContext_RolesSpaceDelimitedString(t *testing.T) {
	p := provider(nil)
	uc, err := buildOIDCUserContext(p, map[string]any{
		"sub": "alice", "roles": "admin user view",
	}, "roles")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(uc.Roles) != 3 {
		t.Errorf("Roles = %v, want 3 elements", uc.Roles)
	}
}

func TestBuildOIDCUserContext_SubMissing(t *testing.T) {
	p := provider(nil)
	_, err := buildOIDCUserContext(p, map[string]any{}, "roles")
	if err == nil || !strings.Contains(err.Error(), "missing_sub") {
		t.Errorf("err = %v, want missing_sub", err)
	}
}

func TestBuildOIDCUserContext_SubTooLong(t *testing.T) {
	p := provider(nil)
	long := strings.Repeat("a", 256)
	_, err := buildOIDCUserContext(p, map[string]any{"sub": long}, "roles")
	if err == nil || !strings.Contains(err.Error(), "invalid_sub") {
		t.Errorf("err = %v, want invalid_sub", err)
	}
}

func TestBuildOIDCUserContext_SubExact255Accepted(t *testing.T) {
	p := provider(nil)
	exact := strings.Repeat("a", 255)
	_, err := buildOIDCUserContext(p, map[string]any{"sub": exact}, "roles")
	if err != nil {
		t.Errorf("255-char sub rejected: %v", err)
	}
}

func TestBuildOIDCUserContext_SubControlCharRejected(t *testing.T) {
	p := provider(nil)
	cases := []string{"\nbad", "good\tbad", "go\x01bad", string([]byte{0x7f})}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := buildOIDCUserContext(p, map[string]any{"sub": s}, "roles")
			if err == nil {
				t.Errorf("sub=%q expected rejection", s)
			}
		})
	}
}

func TestBuildOIDCUserContext_IgnoresAttackerControlledClaims(t *testing.T) {
	p := provider(nil)
	uc, err := buildOIDCUserContext(p, map[string]any{
		"sub":          "alice",
		"caas_user_id": "victim",
		"caas_org_id":  "victim-org",
		"tid":          "victim-tenant",
		"tenant":       "victim-tenant2",
		"org":          "victim-org2",
	}, "roles")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if uc.Tenant.ID == "victim-tenant" || uc.Tenant.ID == "victim-tenant2" {
		t.Errorf("Tenant.ID was overridden by token claim — D23 violation: %q", uc.Tenant.ID)
	}
	if uc.UserID != "oidc:11111111-2222-3333-4444-555555555555:alice" {
		t.Errorf("UserID was overridden by token claim: %q", uc.UserID)
	}
}

func TestBuildOIDCUserContext_SubUnicodeCharsCountAsOne(t *testing.T) {
	p := provider(nil)
	// 200 ASCII + 50 multi-byte = 250 runes but more bytes.
	// Must be accepted.
	sub := strings.Repeat("a", 200) + strings.Repeat("é", 50)
	if utf8.RuneCountInString(sub) != 250 {
		t.Fatalf("test setup: expected 250 runes, got %d", utf8.RuneCountInString(sub))
	}
	_, err := buildOIDCUserContext(p, map[string]any{"sub": sub}, "roles")
	if err != nil {
		t.Errorf("250-rune sub rejected: %v", err)
	}
}

func TestExtractRoles_HandlesAllInputForms(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int // length of returned slice
	}{
		{"nil", nil, 0},
		{"empty-string", "", 0},
		{"space-delimited-string", "admin user view", 3},
		{"string-with-extra-whitespace", "  admin   user  ", 2},
		{"[]string", []string{"admin", "user"}, 2},
		{"[]string-with-empties", []string{"admin", "", "user"}, 2},
		{"[]any-of-strings", []any{"admin", "user"}, 2},
		{"[]any-mixed-types", []any{"admin", 42, "user", nil}, 2},
		{"unsupported-type", map[string]string{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractRoles(c.in)
			if len(got) != c.want {
				t.Errorf("extractRoles(%v) = %v (len %d), want len %d", c.in, got, len(got), c.want)
			}
		})
	}
}

// TestBuildOIDCUserContext_NilOwnerLegalEntityIDRejected verifies the
// defence-in-depth guard: if a provider was somehow persisted with
// OwnerLegalEntityID == uuid.Nil (bypassing the adapter's UUID check),
// buildOIDCUserContext refuses to build a UserContext.
//
// Background: the synthetic "nil tenant" (00000000-...) would silently merge
// users from distinct non-UUID tenants into a shared identity downstream.
func TestBuildOIDCUserContext_NilOwnerLegalEntityIDRejected(t *testing.T) {
	p := &OidcProvider{
		ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		OwnerLegalEntityID: uuid.Nil,
	}
	_, err := buildOIDCUserContext(p, map[string]any{"sub": "alice"}, "roles")
	if err == nil {
		t.Fatal("expected error for nil OwnerLegalEntityID, got nil")
	}
	if !strings.Contains(err.Error(), "nil OwnerLegalEntityID") {
		t.Errorf("error should mention nil OwnerLegalEntityID: %v", err)
	}
}

func TestBuildOIDCUserContext_SubContainingColonAccepted(t *testing.T) {
	// `:` is a legitimate sub character (Auth0 uses e.g. `auth0|abc123`,
	// but other IdPs may use `:`). UserID is opaque downstream per D23.
	p := provider(nil)
	uc, err := buildOIDCUserContext(p, map[string]any{"sub": "a:b:c"}, "roles")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expected := "oidc:11111111-2222-3333-4444-555555555555:a:b:c"
	if uc.UserID != expected {
		t.Errorf("UserID = %q, want %q", uc.UserID, expected)
	}
}
