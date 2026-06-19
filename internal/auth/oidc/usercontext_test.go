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
		// Object-shaped claim (Zitadel projectRoleAssertion etc.): keys are the roles.
		{"map[string]any-zitadel-single", map[string]any{
			"admin": map[string]any{"312orgId": "sioms.localhost"},
		}, 1},
		{"map[string]any-zitadel-multi", map[string]any{
			"sales_rep": map[string]any{"312orgId": "sioms.localhost"},
			"warehouse": map[string]any{"312orgId": "sioms.localhost"},
		}, 2},
		{"map[string]any-empty", map[string]any{}, 0},
		{"map[string]any-with-empty-key", map[string]any{
			"":      map[string]any{"orgId": "x"},
			"admin": map[string]any{"orgId": "y"},
		}, 1},
		{"map[string]any-mixed-value-types", map[string]any{
			"admin":   42,
			"viewer":  true,
			"editor":  nil,
			"manager": map[string]any{"orgId": "x"},
		}, 4},
		// map[string]string is also a legitimate JSON-object decode shape;
		// keys are still the roles, inner string values are ignored.
		{"map[string]string", map[string]string{"admin": "x", "viewer": "y"}, 2},
		// Comma-containing role names are dropped at every shape, so the
		// CloudEvent authclaims comma-join (internal/grpc/cloudevent.go)
		// never sees an ambiguous round-trip. See TestExtractRoles_CommaContainingRolesDropped.
		{"string-token-with-comma-dropped", "admin,m2m view", 1},
		{"[]string-with-comma-role-dropped", []string{"admin", "ops,sales"}, 1},
		{"[]any-with-comma-role-dropped", []any{"admin", "ops,sales"}, 1},
		{"map[string]any-with-comma-key-dropped", map[string]any{
			"admin":     nil,
			"ops,sales": map[string]any{"orgId": "x"},
		}, 1},
		{"map[string]string-with-comma-key-dropped", map[string]string{
			"admin":     "x",
			"ops,sales": "y",
		}, 1},
		// Truly unsupported scalars stay unsupported.
		{"unsupported-type-int", 42, 0},
		{"unsupported-type-bool", true, 0},
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

// TestExtractRoles_ObjectShape verifies that when a JWT claim is decoded as
// a JSON object (e.g. Zitadel's `urn:zitadel:iam:org:project:roles` with
// projectRoleAssertion=true), the top-level keys are the extracted roles
// regardless of the value type underneath. Element-count alone is not
// sufficient — a regression that returned the inner orgId values would
// also produce the right count for a 2-role input. We compare as a set
// because map iteration order is not stable.
func TestExtractRoles_ObjectShape(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string // membership, order-independent
	}{
		{
			name: "zitadel-single-role",
			in: map[string]any{
				"admin": map[string]any{"312orgId": "sioms.localhost"},
			},
			want: []string{"admin"},
		},
		{
			name: "zitadel-multi-role",
			in: map[string]any{
				"sales_rep": map[string]any{"312orgId": "sioms.localhost"},
				"warehouse": map[string]any{"312orgId": "sioms.localhost"},
			},
			want: []string{"sales_rep", "warehouse"},
		},
		{
			name: "empty-key-dropped",
			in: map[string]any{
				"":      map[string]any{"orgId": "x"},
				"admin": map[string]any{"orgId": "y"},
			},
			want: []string{"admin"},
		},
		{
			name: "values-are-ignored-regardless-of-type",
			in: map[string]any{
				"admin":   42,
				"viewer":  true,
				"editor":  nil,
				"manager": map[string]any{"orgId": "x"},
			},
			want: []string{"admin", "viewer", "editor", "manager"},
		},
		{
			name: "map-string-string-also-supported",
			in:   map[string]string{"admin": "x", "viewer": "y"},
			want: []string{"admin", "viewer"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractRoles(c.in)
			if !sameStringSet(got, c.want) {
				t.Errorf("extractRoles(%v) = %v, want set %v", c.in, got, c.want)
			}
		})
	}
}

// sameStringSet returns true iff a and b contain the same elements
// (multiset semantics, order-independent). Tiny — extraction inputs
// are O(10) elements at most.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := map[string]int{}
	cb := map[string]int{}
	for _, s := range a {
		ca[s]++
	}
	for _, s := range b {
		cb[s]++
	}
	if len(ca) != len(cb) {
		return false
	}
	for k, v := range ca {
		if cb[k] != v {
			return false
		}
	}
	return true
}

// TestBuildOIDCUserContext_ZitadelObjectRolesClaim verifies that with a
// Zitadel-shaped object claim and a per-provider rolesClaim pointing at
// it, buildOIDCUserContext extracts the role keys onto UserContext.Roles.
func TestBuildOIDCUserContext_ZitadelObjectRolesClaim(t *testing.T) {
	const zitadelClaim = "urn:zitadel:iam:org:project:roles"
	rc := zitadelClaim
	p := provider(&rc)

	t.Run("single-role", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub": "alice",
			zitadelClaim: map[string]any{
				"admin": map[string]any{"312orgId": "sioms.localhost"},
			},
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sameStringSet(uc.Roles, []string{"admin"}) {
			t.Errorf("Roles = %v, want [admin]", uc.Roles)
		}
	})

	t.Run("multi-role", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub": "alice",
			zitadelClaim: map[string]any{
				"sales_rep": map[string]any{"312orgId": "sioms.localhost"},
				"warehouse": map[string]any{"312orgId": "sioms.localhost"},
			},
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sameStringSet(uc.Roles, []string{"sales_rep", "warehouse"}) {
			t.Errorf("Roles = %v, want set [sales_rep warehouse]", uc.Roles)
		}
	})

	t.Run("empty-object-yields-no-roles", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub":        "alice",
			zitadelClaim: map[string]any{},
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(uc.Roles) != 0 {
			t.Errorf("Roles = %v, want empty slice", uc.Roles)
		}
	})

	t.Run("absent-claim-yields-no-roles", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub": "alice",
			// zitadelClaim intentionally absent
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(uc.Roles) != 0 {
			t.Errorf("Roles = %v, want empty slice", uc.Roles)
		}
	})
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

// TestExtractRoles_CommaContainingRolesDropped pins down the rationale for
// dropping comma-containing role names at extraction time. UserContext.Roles
// is comma-joined into the CloudEvent `authclaims` attribute by
// internal/grpc/cloudevent.go; a role name containing a comma would round-trip
// ambiguously for any consumer that splits on commas. Filtering at the
// extraction boundary keeps the wire format intact and matches the convention
// of every major IdP (Cognito, Zitadel, Auth0, Keycloak), none of which emit
// commas in role names. Validates the drop across every accepted claim shape.
func TestExtractRoles_CommaContainingRolesDropped(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string // membership, order-independent
	}{
		{
			name: "string-space-delimited",
			in:   "admin ops,sales m2m",
			want: []string{"admin", "m2m"},
		},
		{
			name: "string-only-comma-token-yields-empty",
			in:   "ops,sales",
			want: []string{},
		},
		{
			name: "[]string-comma-role-dropped",
			in:   []string{"admin", "ops,sales", "m2m"},
			want: []string{"admin", "m2m"},
		},
		{
			name: "[]any-comma-role-dropped",
			in:   []any{"admin", "ops,sales", "m2m"},
			want: []string{"admin", "m2m"},
		},
		{
			name: "map[string]any-comma-key-dropped",
			in: map[string]any{
				"admin":     map[string]any{"orgId": "a"},
				"ops,sales": map[string]any{"orgId": "b"},
				"m2m":       map[string]any{"orgId": "c"},
			},
			want: []string{"admin", "m2m"},
		},
		{
			name: "map[string]string-comma-key-dropped",
			in: map[string]string{
				"admin":     "a",
				"ops,sales": "b",
				"m2m":       "c",
			},
			want: []string{"admin", "m2m"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractRoles(c.in)
			if !sameStringSet(got, c.want) {
				t.Errorf("extractRoles(%v) = %v, want set %v", c.in, got, c.want)
			}
			for _, r := range got {
				if strings.ContainsRune(r, ',') {
					t.Errorf("comma-containing role leaked: %q", r)
				}
			}
		})
	}
}
