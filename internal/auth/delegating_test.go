package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func generateTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal PKCS8: %v", err)
	}

	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	return string(pemBlock)
}

func TestAuthService_FullFlow(t *testing.T) {
	pemKey := generateTestPEM(t)

	svc := newTestAuthService(t, AuthConfig{
		SigningKeyPEM: pemKey,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})

	// Start test server with AuthService handler.
	server := httptest.NewServer(svc.Handler())
	defer server.Close()

	// Create M2M client directly via store.
	secret, err := svc.M2MClientStore().Create(replicaSystemCtx(), "tenant-1", "TESTCLIENT", "user-1", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("failed to create M2M client: %v", err)
	}

	// Request a token via POST /oauth/token with Basic auth.
	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequest(http.MethodPost, server.URL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("TESTCLIENT", secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var tokenResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("failed to decode token response: %v", err)
	}

	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatal("missing access_token in response")
	}

	// Validate the token in-process via the AuthService's own KeyStore —
	// the production validator's path (no HTTP JWKS fetch).
	validator := NewValidatorFromSource(NewLocalKeySource(svc.KeyStore()), "cyoda")

	uc, err := validator.Validate(accessToken)
	if err != nil {
		t.Fatalf("token validation failed: %v", err)
	}

	if uc.UserID != "user-1" {
		t.Errorf("expected UserID user-1, got %s", uc.UserID)
	}
	if string(uc.Tenant.ID) != "tenant-1" {
		t.Errorf("expected TenantID tenant-1, got %s", uc.Tenant.ID)
	}
	if len(uc.Roles) != 1 || uc.Roles[0] != "ROLE_ADMIN" {
		t.Errorf("expected roles [ROLE_ADMIN], got %v", uc.Roles)
	}
}

func TestDelegatingAuthenticator_ValidToken(t *testing.T) {
	pemKey := generateTestPEM(t)

	svc := newTestAuthService(t, AuthConfig{
		SigningKeyPEM: pemKey,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})

	// Get active key pair for signing.
	kp, signer, err := svc.KeyStore().Signer("client")
	if err != nil {
		t.Fatalf("failed to get the signing key pair: %v", err)
	}

	// Sign a token directly.
	now := time.Now()
	claims := map[string]any{
		"sub":          "test-client",
		"iss":          "cyoda",
		"caas_user_id": "user-42",
		"caas_org_id":  "tenant-42",
		"scopes":       []string{"ROLE_USER"},
		"exp":          float64(now.Add(1 * time.Hour).Unix()),
		"iat":          float64(now.Unix()),
	}

	token, err := Sign(context.Background(), claims, signer, kp.KID)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	// Create the DelegatingAuthenticator.
	validator := NewValidatorFromSource(NewLocalKeySource(svc.KeyStore()), "cyoda")
	authn := NewDelegatingAuthenticator(validator)

	// Build an HTTP request with a Bearer token.
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	uc, err := authn.Authenticate(context.Background(), req)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}

	if uc.UserID != "user-42" {
		t.Errorf("expected UserID user-42, got %s", uc.UserID)
	}
	if string(uc.Tenant.ID) != "tenant-42" {
		t.Errorf("expected TenantID tenant-42, got %s", uc.Tenant.ID)
	}
	if len(uc.Roles) != 1 || uc.Roles[0] != "ROLE_USER" {
		t.Errorf("expected roles [ROLE_USER], got %v", uc.Roles)
	}
}

func TestDelegatingAuthenticator_NoToken(t *testing.T) {
	validator := newTestJWKSValidator(t, "cyoda")
	authn := NewDelegatingAuthenticator(validator)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	_, err := authn.Authenticate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing Authorization header")
	}

	// The caller-facing message is uniform; the specific
	// reason ("missing-header") goes to the server log only.
	if err.Error() != "authentication failed" {
		t.Errorf("unexpected error message: %s", err.Error())
	}
	if !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("errors.Is(err, ErrAuthenticationFailed) = false; err = %v", err)
	}
}

func TestDelegatingAuthenticator_InvalidToken(t *testing.T) {
	validator := newTestJWKSValidator(t, "cyoda")
	authn := NewDelegatingAuthenticator(validator)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.value")

	_, err := authn.Authenticate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}

	// The caller-facing message is uniform.
	if err.Error() != "authentication failed" {
		t.Errorf("unexpected error message: %s", err.Error())
	}
	if !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("errors.Is(err, ErrAuthenticationFailed) = false; err = %v", err)
	}
}

func TestDelegatingAuthenticator_NonBearerScheme(t *testing.T) {
	validator := newTestJWKSValidator(t, "cyoda")
	authn := NewDelegatingAuthenticator(validator)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

	_, err := authn.Authenticate(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-Bearer scheme")
	}

	// The caller-facing message is uniform.
	if err.Error() != "authentication failed" {
		t.Errorf("unexpected error message: %s", err.Error())
	}
	if !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("errors.Is(err, ErrAuthenticationFailed) = false; err = %v", err)
	}
}
