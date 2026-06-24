package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func generateTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
}

func TestIntegration_JWTMode_CreateM2M_GetToken_ValidateToken(t *testing.T) {
	pemKey := generateTestPEM(t)

	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pemKey,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	// Start test server with auth routes
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	// Create M2M client via store (normally would be via API, but testing the flow)
	secret, err := svc.M2MClientStore().Create("test-app", "tenant-1", "user-1", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("Create M2M client: %v", err)
	}

	// Issue token via POST /oauth/token
	body := "grant_type=client_credentials"
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("test-app:"+secret)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /oauth/token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tokenResp.AccessToken == "" {
		t.Fatal("empty access_token")
	}
	if tokenResp.TokenType != "Bearer" {
		t.Fatalf("expected token_type Bearer, got %s", tokenResp.TokenType)
	}

	// Validate token using JWKSValidator pointed at our test server
	validator := auth.NewJWKSValidator(srv.URL+"/.well-known/jwks.json", "cyoda", 5*time.Minute)
	uc, err := validator.Validate(tokenResp.AccessToken)
	if err != nil {
		t.Fatalf("Validate token: %v", err)
	}

	if uc.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q", uc.UserID, "user-1")
	}
	if string(uc.Tenant.ID) != "tenant-1" {
		t.Errorf("TenantID = %q, want %q", uc.Tenant.ID, "tenant-1")
	}
	if len(uc.Roles) != 1 || uc.Roles[0] != "ROLE_ADMIN" {
		t.Errorf("Roles = %v, want [ROLE_ADMIN]", uc.Roles)
	}
}

// TestAuthService_DeterministicKID verifies that two AuthService instances
// created with the same RSA key produce the same KID.
func TestAuthService_DeterministicKID(t *testing.T) {
	sharedPEM := generateTestPEM(t)

	svcA, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: sharedPEM, Issuer: "cyoda", ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService A: %v", err)
	}

	svcB, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: sharedPEM, Issuer: "cyoda", ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService B: %v", err)
	}

	kidA := svcA.SigningKID()
	kidB := svcB.SigningKID()

	if kidA == "" {
		t.Fatal("KID A is empty")
	}
	if kidA != kidB {
		t.Errorf("KID mismatch: A=%q B=%q — must be deterministic for multi-node", kidA, kidB)
	}
}

// TestIntegration_MultiNode_CrossNodeTokenValidation verifies that a JWT token
// issued by one node can be validated by a different node sharing the same RSA key.
// Regression test: previously each node generated a random KID, so JWKS lookup
// by kid failed across nodes even though the actual key was identical.
func TestIntegration_MultiNode_CrossNodeTokenValidation(t *testing.T) {
	// Same key for both nodes — simulates shared CYODA_JWT_SIGNING_KEY
	sharedPEM := generateTestPEM(t)

	// Node A
	svcA, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: sharedPEM,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService (node A): %v", err)
	}
	srvA := httptest.NewServer(svcA.Handler())
	defer srvA.Close()

	// Node B — same key, separate instance
	svcB, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: sharedPEM,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService (node B): %v", err)
	}
	srvB := httptest.NewServer(svcB.Handler())
	defer srvB.Close()

	// Create M2M client on node A and issue a token
	secret, err := svcA.M2MClientStore().Create("test-app", "tenant-1", "user-1", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("Create M2M client on node A: %v", err)
	}

	body := "grant_type=client_credentials"
	req, _ := http.NewRequest("POST", srvA.URL+"/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("test-app:"+secret)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /oauth/token on node A: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from node A, got %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}

	// Validate the token issued by node A using node B's JWKS endpoint.
	// This fails if KID is random per node (the original bug).
	validatorB := auth.NewJWKSValidator(srvB.URL+"/.well-known/jwks.json", "cyoda", 5*time.Minute)
	uc, err := validatorB.Validate(tokenResp.AccessToken)
	if err != nil {
		t.Fatalf("Node B failed to validate token from node A: %v", err)
	}

	if uc.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q", uc.UserID, "user-1")
	}
	if string(uc.Tenant.ID) != "tenant-1" {
		t.Errorf("TenantID = %q, want %q", uc.Tenant.ID, "tenant-1")
	}
}

func TestIntegration_RequestBodySizeLimit(t *testing.T) {
	pemKey := generateTestPEM(t)

	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pemKey,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	// Create an M2M client so we can authenticate
	secret, err := svc.M2MClientStore().Create("test-app", "tenant-1", "user-1", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("Create M2M client: %v", err)
	}

	t.Run("token endpoint rejects oversized body", func(t *testing.T) {
		// Send a body larger than 1MB to the token endpoint
		oversized := strings.Repeat("x", 1<<20+1)
		req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(oversized))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("test-app:"+secret)))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /oauth/token: %v", err)
		}
		defer resp.Body.Close()

		// http.MaxBytesReader triggers 413 Request Entity Too Large
		if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 413 or 400, got %d", resp.StatusCode)
		}
	})
}

func TestIntegration_JWTMode_UnauthenticatedRequest(t *testing.T) {
	pemKey := generateTestPEM(t)

	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pemKey,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	validator := auth.NewJWKSValidator(srv.URL+"/.well-known/jwks.json", "cyoda", 5*time.Minute)
	authenticator := auth.NewDelegatingAuthenticator(validator)

	// Request without auth header
	req := httptest.NewRequest("GET", "/api/v1/entity", nil)
	_, err = authenticator.Authenticate(req.Context(), req)
	if err == nil {
		t.Fatal("expected error for unauthenticated request")
	}
}

func TestIntegration_JWTMode_InvalidToken(t *testing.T) {
	pemKey := generateTestPEM(t)

	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: pemKey,
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
	})
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	validator := auth.NewJWKSValidator(srv.URL+"/.well-known/jwks.json", "cyoda", 5*time.Minute)
	authenticator := auth.NewDelegatingAuthenticator(validator)

	// Request with invalid token
	req := httptest.NewRequest("GET", "/api/v1/entity", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	_, err = authenticator.Authenticate(req.Context(), req)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}
