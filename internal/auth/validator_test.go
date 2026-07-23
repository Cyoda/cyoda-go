package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func setupTestJWKS(t *testing.T) (*rsa.PrivateKey, string, *httptest.Server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	kid := "test-kid"

	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	jwksJSON := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"%s","use":"sig","alg":"RS256","n":"%s","e":"%s"}]}`, kid, n, e)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwksJSON))
	}))

	return key, kid, srv
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	token, err := auth.Sign(claims, key, kid)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return token
}

func TestJWKSValidator_ValidToken(t *testing.T) {
	key, kid, srv := setupTestJWKS(t)
	defer srv.Close()

	issuer := "test-issuer"
	v := auth.NewJWKSValidator(srv.URL, issuer, 5*time.Minute)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
		"scopes":       []any{"admin", "read"},
	}

	token := signTestToken(t, key, kid, claims)

	uc, err := v.Validate(token)
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
	key, kid, srv := setupTestJWKS(t)
	defer srv.Close()

	issuer := "test-issuer"
	v := auth.NewJWKSValidator(srv.URL, issuer, 5*time.Minute)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(-time.Hour).Unix()),
		"iat":          float64(time.Now().Add(-2 * time.Hour).Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
		"scopes":       []any{"admin"},
	}

	token := signTestToken(t, key, kid, claims)

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestJWKSValidator_UnknownKid(t *testing.T) {
	key, _, srv := setupTestJWKS(t)
	defer srv.Close()

	issuer := "test-issuer"
	v := auth.NewJWKSValidator(srv.URL, issuer, 5*time.Minute)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
	}

	// Sign with a kid that is not in the JWKS
	token := signTestToken(t, key, "unknown-kid", claims)

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for unknown kid, got nil")
	}
}

func TestJWKSValidator_InvalidSignature(t *testing.T) {
	_, kid, srv := setupTestJWKS(t)
	defer srv.Close()

	issuer := "test-issuer"
	v := auth.NewJWKSValidator(srv.URL, issuer, 5*time.Minute)

	// Sign with a different key than what is served by JWKS
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

	_, err = v.Validate(token)
	if err == nil {
		t.Fatal("expected error for invalid signature, got nil")
	}
}

func TestJWKSValidator_CacheRefresh(t *testing.T) {
	// Start with one key served by JWKS
	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key1: %v", err)
	}
	kid1 := "kid-1"

	key2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key2: %v", err)
	}
	kid2 := "kid-2"

	// Track which keys to serve; start with key1 only
	var serveKey2 atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		keys := []string{
			fmt.Sprintf(`{"kty":"RSA","kid":"%s","use":"sig","alg":"RS256","n":"%s","e":"%s"}`,
				kid1,
				base64.RawURLEncoding.EncodeToString(key1.N.Bytes()),
				base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key1.E)).Bytes())),
		}

		if serveKey2.Load() {
			keys = append(keys, fmt.Sprintf(`{"kty":"RSA","kid":"%s","use":"sig","alg":"RS256","n":"%s","e":"%s"}`,
				kid2,
				base64.RawURLEncoding.EncodeToString(key2.N.Bytes()),
				base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key2.E)).Bytes())))
		}

		fmt.Fprintf(w, `{"keys":[%s]}`, join(keys, ","))
	}))
	defer srv.Close()

	issuer := "test-issuer"
	// Use a very short TTL so cache goes stale quickly
	v := auth.NewJWKSValidator(srv.URL, issuer, 1*time.Millisecond)

	claims := map[string]any{
		"iss":          issuer,
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
		"caas_user_id": "user-42",
		"caas_org_id":  "org-7",
		"scopes":       []any{"admin"},
	}

	// Validate with key1 — should work
	token1 := signTestToken(t, key1, kid1, claims)
	if _, err := v.Validate(token1); err != nil {
		t.Fatalf("expected key1 validation to succeed: %v", err)
	}

	// Token signed with key2 — should fail because key2 not yet served
	token2 := signTestToken(t, key2, kid2, claims)
	_, err = v.Validate(token2)
	if err == nil {
		t.Fatal("expected key2 validation to fail before key2 is served")
	}

	// Now serve key2 and wait for cache to go stale
	serveKey2.Store(true)
	time.Sleep(5 * time.Millisecond)

	// Now key2 should be found after cache refresh
	uc, err := v.Validate(token2)
	if err != nil {
		t.Fatalf("expected key2 validation to succeed after refresh: %v", err)
	}
	if uc.UserID != "user-42" {
		t.Errorf("expected UserID user-42, got %s", uc.UserID)
	}
}

func TestJWKSValidator_PrincipalKind(t *testing.T) {
	key, kid, srv := setupTestJWKS(t)
	defer srv.Close()

	issuer := "test-issuer"
	v := auth.NewJWKSValidator(srv.URL, issuer, 5*time.Minute)

	baseClaims := func() map[string]any {
		return map[string]any{
			"iss":          issuer,
			"exp":          float64(time.Now().Add(time.Hour).Unix()),
			"iat":          float64(time.Now().Unix()),
			"caas_user_id": "user-42",
			"caas_org_id":  "org-7",
		}
	}

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantKnd spi.PrincipalKind
	}{
		{
			name: "user_roles present",
			mutate: func(c map[string]any) {
				c["user_roles"] = []any{"ROLE_ADMIN"}
			},
			wantKnd: spi.PrincipalUser,
		},
		{
			name: "user_roles present but empty array — key presence, not len",
			mutate: func(c map[string]any) {
				c["user_roles"] = []any{}
			},
			wantKnd: spi.PrincipalUser,
		},
		{
			name: "scopes only — service",
			mutate: func(c map[string]any) {
				c["scopes"] = []any{"read"}
			},
			wantKnd: spi.PrincipalService,
		},
		{
			name: "both user_roles and scopes present — user",
			mutate: func(c map[string]any) {
				c["user_roles"] = []any{"ROLE_ADMIN"}
				c["scopes"] = []any{"read"}
			},
			wantKnd: spi.PrincipalUser,
		},
		{
			name:    "neither claim present — attribution-safe default user",
			mutate:  func(c map[string]any) {},
			wantKnd: spi.PrincipalUser,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := baseClaims()
			tt.mutate(claims)
			token := signTestToken(t, key, kid, claims)

			uc, err := v.Validate(token)
			if err != nil {
				t.Fatalf("Validate failed: %v", err)
			}
			if uc.Kind != tt.wantKnd {
				t.Errorf("Kind = %q, want %q", uc.Kind, tt.wantKnd)
			}
		})
	}
}

func join(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for _, s := range strs[1:] {
		result += sep + s
	}
	return result
}
