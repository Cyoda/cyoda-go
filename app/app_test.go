package app_test

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
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestConfig_NoBootstrapClient verifies that no credential is defined by
// configuration except the signing key (`cyoda token` signs the first admin
// token): a jwt-mode app started with the former bootstrap-client variables
// set creates no client from them, and a client_credentials grant with them
// is refused.
func TestConfig_NoBootstrapClient(t *testing.T) {
	t.Setenv("CYODA_BOOTSTRAP_CLIENT_ID", "leftover")
	t.Setenv("CYODA_BOOTSTRAP_CLIENT_SECRET", "leftover-secret")
	t.Setenv("CYODA_BOOTSTRAP_TENANT_ID", "leftover-tenant")
	t.Setenv("CYODA_BOOTSTRAP_USER_ID", "leftover-user")
	t.Setenv("CYODA_BOOTSTRAP_ROLES", "ROLE_ADMIN,ROLE_M2M")
	a := jwtApp(t)
	if _, err := a.AuthService().M2MClientStore().Authenticate(context.Background(), "leftover", "leftover-secret"); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("M2MClientStore().Authenticate(leftover) err = %v, want ErrInvalidClient", err)
	}
	if l, err := a.AuthService().M2MClientStore().List(context.Background(), "leftover-tenant"); err != nil || len(l) != 0 {
		t.Fatalf("leftover-tenant clients: %v %v, want none", l, err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("leftover", "leftover-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("client_credentials with the leftover client: status %d, want 401", resp.StatusCode)
	}
}

func TestHealthEndpoint(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "UP" {
		t.Fatalf("expected status UP, got %s", body["status"])
	}
}

func TestDIWiring(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)

	checks := []struct {
		name string
		ok   bool
	}{
		{"StoreFactory", a.StoreFactory() != nil},
		{"TransactionManager", a.TransactionManager() != nil},
		{"AuthenticationService", a.AuthenticationService() != nil},
		{"WorkflowEngine", a.WorkflowEngine() != nil},
		{"SearchService", a.SearchService() != nil},
		{"AuditService", a.AuditService() != nil},
		{"StoreFactory.MessageStore", func() bool {
			ctx := ctxWithTenant("test-tenant")
			ms, err := a.StoreFactory().MessageStore(ctx)
			return err == nil && ms != nil
		}()},
		{"ClusterService", a.ClusterService() != nil},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !c.ok {
				t.Errorf("%s is nil — not wired", c.name)
			}
		})
	}
}

func TestScalarDocsEndpoint(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/docs")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html, got %s", ct)
	}
}

func TestOpenAPISpecEndpoint(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %s", ct)
	}
}

func TestAPIEndpointsReturn501(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/entity/stats"},
		{"POST", "/search/direct/TestEntity/1"},
		{"GET", "/model/"},
		{"GET", "/account"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, _ := http.NewRequest(ep.method, srv.URL+ep.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
				t.Errorf("endpoint not registered: got %d", resp.StatusCode)
			}
		})
	}
}

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

func jwtApp(t *testing.T) *app.App {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = generateTestPEM(t)
	cfg.IAM.JWTIssuer = "cyoda"
	cfg.IAM.JWTExpiry = 3600
	return app.New(cfg)
}

// TestAuthAdminEndpointsRequireAuth verifies that auth management endpoints
// (key management, M2M client management, trusted keys) reject unauthenticated
// requests with 401. Only token and JWKS endpoints should be public.
func TestAuthAdminEndpointsRequireAuth(t *testing.T) {
	a := jwtApp(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// These endpoints MUST require authentication (currently they don't — this test should fail).
	adminEndpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/oauth/keys/keypair"},
		{"DELETE", "/oauth/keys/keypair/some-kid"},
		{"POST", "/oauth/keys/keypair/some-kid/invalidate"},
		{"POST", "/oauth/keys/trusted"},
		{"GET", "/clients"},
		{"POST", "/clients"},
		{"DELETE", "/clients/some-client"},
		{"PUT", "/clients/some-client/secret"},
	}

	for _, ep := range adminEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, _ := http.NewRequest(ep.method, srv.URL+ep.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("expected 401 Unauthorized for unauthenticated %s %s, got %d",
					ep.method, ep.path, resp.StatusCode)
			}
		})
	}
}

// TestAuthPublicEndpointsNoAuth verifies that token and JWKS endpoints
// remain accessible without authentication.
func TestAuthPublicEndpointsNoAuth(t *testing.T) {
	a := jwtApp(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// JWKS should return 200 without auth
	resp, err := http.Get(srv.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("JWKS request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("JWKS expected 200, got %d", resp.StatusCode)
	}

	// Token endpoint should return 401 (bad credentials), not 404 or 500
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	resp.Body.Close()
	// Without credentials, token endpoint returns 401 (expected behavior)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token endpoint expected 401 for missing creds, got %d", resp.StatusCode)
	}
}

// TestMockMode_TokenEndpoint_Returns501: mock IAM mode mounts no token
// handler, so POST /oauth/token reaches the generated router, which answers
// 501 NOT_IMPLEMENTED: mock mode issues no token.
func TestMockMode_TokenEndpoint_Returns501(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusNotImplemented || body.Properties["errorCode"] != "NOT_IMPLEMENTED" {
		t.Fatalf("mock-mode POST /oauth/token: status %d errorCode %v, want 501 NOT_IMPLEMENTED",
			resp.StatusCode, body.Properties["errorCode"])
	}
}

// jwtAppWithKey is jwtApp, also returning the signing key.
func jwtAppWithKey(t *testing.T) (*app.App, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	cfg.IAM.JWTIssuer = "cyoda"
	cfg.IAM.JWTExpiry = 3600
	return app.New(cfg), key
}

// mintAppToken signs a token for tenant acme with the given roles.
func mintAppToken(t *testing.T, key *rsa.PrivateKey, roles ...string) string {
	t.Helper()
	tok, err := auth.MintOperatorToken(context.Background(), key, auth.OperatorTokenRequest{
		Tenant: "acme", UserID: "tester", Roles: roles, TTL: 5 * time.Minute, Issuer: "cyoda"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func ctxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func TestMultiTenantAPIIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()

	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, err := factory.EntityStore(ctxA)
	if err != nil {
		t.Fatalf("failed to get store for tenant-A: %v", err)
	}
	storeB, err := factory.EntityStore(ctxB)
	if err != nil {
		t.Fatalf("failed to get store for tenant-B: %v", err)
	}

	entityA := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-001",
			TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{"tenant": "A"}`),
	}
	storeA.Save(ctxA, entityA)

	_, err = storeB.Get(ctxB, "e-001")
	if err == nil {
		t.Error("tenant-B should not see tenant-A's entity through the factory")
	}

	got, err := storeA.Get(ctxA, "e-001")
	if err != nil {
		t.Fatalf("tenant-A should see its own entity: %v", err)
	}
	if string(got.Data) != `{"tenant": "A"}` {
		t.Errorf("unexpected data: %s", got.Data)
	}
}
