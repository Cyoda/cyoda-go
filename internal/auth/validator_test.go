package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// setupTestJWKS generates an RSA key pair for a fixed test kid. Callers wrap
// it in a staticKeySource to exercise JWKSValidator without an HTTP JWKS
// server.
func setupTestJWKS(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	return key, "test-kid"
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return token
}

func TestJWKSValidator_ValidToken(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
		"scopes":       []any{"admin", "read"},
	}

	token := signTestToken(t, key, kid, claims)

	uc, _, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	if uc.UserID != "user-42" {
		t.Errorf("expected UserID user-42, got %s", uc.UserID)
	}
	if uc.UserName != "user-42" {
		t.Errorf("expected UserName user-42, got %s", uc.UserName)
	}
	if uc.Tenant.ID != spi.TenantID("org-7") {
		t.Errorf("expected Tenant.ID org-7, got %s", uc.Tenant.ID)
	}
	if uc.Tenant.Name != "org-7" {
		t.Errorf("expected Tenant.Name org-7, got %s", uc.Tenant.Name)
	}
	if len(uc.Roles) != 2 || uc.Roles[0] != "admin" || uc.Roles[1] != "read" {
		t.Errorf("expected roles [admin read], got %v", uc.Roles)
	}
}

func TestJWKSValidator_ExpiredToken(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(-time.Hour).Unix()),
		"iat":          float64(time.Now().Add(-2 * time.Hour).Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
		"scopes":       []any{"admin"},
	}

	token := signTestToken(t, key, kid, claims)

	_, _, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestJWKSValidator_UnknownKid(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	// Sign with a kid that is not in the key source.
	token := signTestToken(t, key, "unknown-kid", claims)

	_, _, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for unknown kid, got nil")
	}
}

func TestJWKSValidator_InvalidSignature(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	// Sign with a different key than what the key source resolves for kid.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate other key: %v", err)
	}

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	token := signTestToken(t, otherKey, kid, claims)

	_, _, err = v.Validate(token)
	if err == nil {
		t.Fatal("expected error for invalid signature, got nil")
	}
}

// TestValidator_RejectsTenantOutsideGrammar pins the tenant door: the caas_org_id claim
// is the one place a tenant id enters cyoda-go on a request, covering HTTP and
// gRPC alike, so a claim outside the grammar must not produce a UserContext.
func TestValidator_RejectsTenantOutsideGrammar(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	for name, org := range map[string]string{
		"traversal": "../victim",
		"dotdot":    "..",
		"slash":     "a/b",
		"colon":     "a:b",
		"newline":   "tenant\ninjected",
		"nul":       "tenant\x00",
		"too-long":  strings.Repeat("x", 101),
	} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"iss":          issuer,
				"exp":          float64(time.Now().Add(time.Hour).Unix()),
				"iat":          float64(time.Now().Unix()),
				"caas_user_id": "user-1",
				"caas_org_id":  org,
				"scopes":       []any{"read"},
			}
			tok := signTestToken(t, key, kid, claims)

			uc, _, err := v.Validate(tok)
			if err == nil {
				t.Fatalf("Validate accepted tenant %q, got UserContext %+v", org, uc)
			}
			if uc != nil {
				t.Errorf("Validate returned a UserContext alongside an error: %+v", uc)
			}
			if strings.Contains(err.Error(), org) {
				t.Errorf("validator error echoes the rejected tenant: %q", err.Error())
			}
		})
	}
}

// TestValidator_AcceptsShippedTenantShapes is the regression half: the grammar
// must not lock out anything that authenticates today.
func TestValidator_AcceptsShippedTenantShapes(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	for _, org := range []string{
		"SYSTEM",
		"plain-tenant",
		"mock-tenant",
		"tenant-abc-123",
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
	} {
		t.Run(org, func(t *testing.T) {
			claims := map[string]any{
				"iss":          issuer,
				"exp":          float64(time.Now().Add(time.Hour).Unix()),
				"iat":          float64(time.Now().Unix()),
				"caas_user_id": "user-1",
				"caas_org_id":  org,
				"scopes":       []any{"read"},
			}
			tok := signTestToken(t, key, kid, claims)

			uc, _, err := v.Validate(tok)
			if err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", org, err)
			}
			if string(uc.Tenant.ID) != org {
				t.Errorf("tenant = %q, want %q", uc.Tenant.ID, org)
			}
		})
	}
}

// TestValidator_RejectsUserClaimOutsideCheck pins the first-party user claim:
// caas_user_id / sub is attacker-chosen, lands in slog and audit
// attribution, and must meet common.ValidateUserID's length+control-char bar.
func TestValidator_RejectsUserClaimOutsideCheck(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	cases := map[string]string{
		"newline":  "user\ninjected",
		"nul":      "user\x00",
		"cr":       "user\r",
		"tab":      "user\tid",
		"del":      "user\x7f",
		"too-long": strings.Repeat("u", 256),
	}
	for name, user := range cases {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"iss":          issuer,
				"exp":          float64(time.Now().Add(time.Hour).Unix()),
				"iat":          float64(time.Now().Unix()),
				"caas_user_id": user,
				"caas_org_id":  "org-7",
				"scopes":       []any{"read"},
			}
			tok := signTestToken(t, key, kid, claims)
			uc, _, err := v.Validate(tok)
			if err == nil {
				t.Fatalf("Validate accepted user id %q, got UserContext %+v", user, uc)
			}
			if uc != nil {
				t.Errorf("Validate returned a UserContext alongside an error: %+v", uc)
			}
			if strings.Contains(err.Error(), user) {
				t.Errorf("validator error echoes the rejected user id: %q", err.Error())
			}
			if !errors.Is(err, common.ErrInvalidUserID) {
				t.Errorf("err = %v, want it to wrap common.ErrInvalidUserID", err)
			}
		})
	}
}

func TestValidator_RejectsControlCharInSubFallback(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	bad := "sub\nvalue"
	claims := map[string]any{
		"iss":         issuer,
		"exp":         float64(time.Now().Add(time.Hour).Unix()),
		"iat":         float64(time.Now().Unix()),
		"sub":         bad,
		"caas_org_id": "org-7",
		"scopes":      []any{"read"},
	}
	tok := signTestToken(t, key, kid, claims)
	_, _, err := v.Validate(tok)
	if err == nil {
		t.Fatal("Validate accepted control character in sub fallback")
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("validator error echoes the rejected sub: %q", err.Error())
	}
	if !errors.Is(err, common.ErrInvalidUserID) {
		t.Errorf("err = %v, want it to wrap common.ErrInvalidUserID", err)
	}
}

// A first-party token cannot carry the reserved user id "system", in any
// letter case, from either caas_user_id or the sub fallback: it would name
// the platform system principal.
func TestValidator_RejectsReservedSystemUserID(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	const spoof = "SYSTEM"
	for name, claim := range map[string]string{"caas_user_id": "caas_user_id", "sub": "sub"} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"iss":         issuer,
				"exp":         float64(time.Now().Add(time.Hour).Unix()),
				"iat":         float64(time.Now().Unix()),
				claim:         spoof,
				"caas_org_id": "org-7",
				"scopes":      []any{"read"},
			}
			uc, _, err := v.Validate(signTestToken(t, key, kid, claims))
			if err == nil {
				t.Fatalf("Validate accepted the reserved id as %q", uc.UserID)
			}
			if !errors.Is(err, common.ErrInvalidUserID) {
				t.Errorf("err = %v, want it to wrap common.ErrInvalidUserID", err)
			}
		})
	}
}

func TestValidator_AcceptsShippedUserIDShapes(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)

	for _, user := range []string{
		"user-42",
		"user-1",
		strings.Repeat("a", 255),
		"alice@example.com",
		"provider-style/sub",
		"用户",
	} {
		t.Run(user, func(t *testing.T) {
			claims := map[string]any{
				"iss":          issuer,
				"exp":          float64(time.Now().Add(time.Hour).Unix()),
				"iat":          float64(time.Now().Unix()),
				"caas_user_id": user,
				"caas_org_id":  "org-7",
				"scopes":       []any{"read"},
			}
			tok := signTestToken(t, key, kid, claims)
			uc, _, err := v.Validate(tok)
			if err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", user, err)
			}
			if uc.UserID != user {
				t.Errorf("UserID = %q, want %q", uc.UserID, user)
			}
		})
	}
}

// A caas_user_id that is present but fails the check does not fall back to
// sub: the token named its user, and that user is not admitted.
func TestValidator_InvalidUserClaimDoesNotFallBackToSub(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user\nx",
		"sub":          "valid-sub",
		"caas_org_id":  "org-7",
		"scopes":       []any{"read"},
	}
	uc, _, err := v.Validate(signTestToken(t, key, kid, claims))
	if err == nil {
		t.Fatalf("Validate accepted the token as %q, want a rejection", uc.UserID)
	}
	if !errors.Is(err, common.ErrInvalidUserID) {
		t.Errorf("err = %v, want it to wrap common.ErrInvalidUserID", err)
	}
}

// A caas_user_id that is present but empty or not a string is rejected.
// Treating it as absent would substitute sub for the identity the token
// actually carries. Only an absent caas_user_id falls back to sub.
func TestValidator_RejectsMalformedPresentUserClaim(t *testing.T) {
	key, kid := setupTestJWKS(t)

	issuer := "test-issuer"
	v := auth.NewValidatorFromSource(staticKeySource{kid: &key.PublicKey}, issuer)
	for name, val := range map[string]any{
		"number": float64(42),
		"bool":   true,
		"array":  []any{"a"},
		"object": map[string]any{"id": "a"},
		"null":   nil,
		"empty":  "",
	} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"iss":          issuer,
				"exp":          float64(time.Now().Add(time.Hour).Unix()),
				"iat":          float64(time.Now().Unix()),
				"caas_user_id": val,
				"sub":          "valid-sub",
				"caas_org_id":  "org-7",
				"scopes":       []any{"read"},
			}
			uc, _, err := v.Validate(signTestToken(t, key, kid, claims))
			if err == nil {
				t.Fatalf("Validate accepted a %s caas_user_id as %q, want a rejection", name, uc.UserID)
			}
			if !errors.Is(err, common.ErrInvalidUserID) {
				t.Errorf("err = %v, want it to wrap common.ErrInvalidUserID", err)
			}
		})
	}
}
