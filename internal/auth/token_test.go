package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// testTokenEnv holds shared test fixtures for token endpoint tests.
type testTokenEnv struct {
	keyStore        *auth.InMemoryKeyStore
	trustedKeyStore *auth.InMemoryTrustedKeyStore
	m2mStore        *auth.InMemoryM2MClientStore
	handler         http.Handler
	clientID        string
	clientSecret    string
	signingKey      *rsa.PrivateKey
	trustedKey      *rsa.PrivateKey
	trustedKID      string
	tenantID        string
}

func setupTokenEnv(t *testing.T) *testTokenEnv {
	t.Helper()

	keyStore := auth.NewInMemoryKeyStore()
	trustedKeyStore := auth.NewInMemoryTrustedKeyStore()
	m2mStore := auth.NewInMemoryM2MClientStore()

	// Generate signing key pair for the token endpoint.
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate signing key: %v", err)
	}
	err = keyStore.Save(&auth.KeyPair{
		KID:        "signing-kid-1",
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &signingKey.PublicKey,
		PrivateKey: signingKey,
		Active:     true,
		ValidFrom:  time.Now(),
	}, auth.RotateOptions{})
	if err != nil {
		t.Fatalf("failed to save signing key: %v", err)
	}

	// Generate trusted external key (simulates an external IdP).
	trustedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate trusted key: %v", err)
	}
	trustedKID := "trusted-kid-1"
	err = trustedKeyStore.Register(&auth.TrustedKey{
		KID:       trustedKID,
		PublicKey: &trustedKey.PublicKey,
		Audience:  "cyoda-go",
		Active:    true,
		ValidFrom: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("failed to register trusted key: %v", err)
	}

	// Create M2M client.
	tenantID := "tenant-abc"
	clientID := "test-m2m-client"
	clientSecret, err := m2mStore.Create(clientID, tenantID, "user-123", []string{"admin", "reader"})
	if err != nil {
		t.Fatalf("failed to create M2M client: %v", err)
	}

	handler := auth.NewTokenHandler(keyStore, trustedKeyStore, m2mStore, "cyoda", 3600)

	return &testTokenEnv{
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mStore:        m2mStore,
		handler:         handler,
		clientID:        clientID,
		clientSecret:    clientSecret,
		signingKey:      signingKey,
		trustedKey:      trustedKey,
		trustedKID:      trustedKID,
		tenantID:        tenantID,
	}
}

func basicAuth(clientID, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(clientID+":"+secret))
}

func makeTokenRequest(grantType, authHeader string, extraForm url.Values) *http.Request {
	form := url.Values{}
	form.Set("grant_type", grantType)
	for k, vs := range extraForm {
		for _, v := range vs {
			form.Set(k, v)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func decodeResponse(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

// signSubjectToken creates a JWT signed with the trusted key, simulating an external IdP token.
func signSubjectToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	token, err := auth.Sign(claims, key, kid)
	if err != nil {
		t.Fatalf("failed to sign subject token: %v", err)
	}
	return token
}

func TestTokenClientCredentialsValid(t *testing.T) {
	env := setupTokenEnv(t)

	req := makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)

	accessToken, ok := resp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatal("expected non-empty access_token")
	}
	if resp["token_type"] != "Bearer" {
		t.Errorf("expected token_type Bearer, got %v", resp["token_type"])
	}
	if resp["expires_in"] != float64(3600) {
		t.Errorf("expected expires_in 3600, got %v", resp["expires_in"])
	}

	// Verify the token is valid and contains expected claims.
	parsed, err := auth.Parse(accessToken)
	if err != nil {
		t.Fatalf("failed to parse access token: %v", err)
	}
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &env.signingKey.PublicKey); err != nil {
		t.Fatalf("token signature verification failed: %v", err)
	}

	if parsed.Claims["sub"] != env.clientID {
		t.Errorf("expected sub %q, got %v", env.clientID, parsed.Claims["sub"])
	}
	if parsed.Claims["iss"] != "cyoda" {
		t.Errorf("expected iss cyoda, got %v", parsed.Claims["iss"])
	}
	if parsed.Claims["caas_user_id"] != "user-123" {
		t.Errorf("expected caas_user_id user-123, got %v", parsed.Claims["caas_user_id"])
	}
	if parsed.Claims["caas_org_id"] != "tenant-abc" {
		t.Errorf("expected caas_org_id tenant-abc, got %v", parsed.Claims["caas_org_id"])
	}
	if parsed.Claims["caas_tier"] != "unlimited" {
		t.Errorf("expected caas_tier unlimited, got %v", parsed.Claims["caas_tier"])
	}
}

func TestTokenClientCredentialsInvalidSecret(t *testing.T) {
	env := setupTokenEnv(t)

	req := makeTokenRequest("client_credentials", basicAuth(env.clientID, "wrong-secret"), nil)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_client" {
		t.Errorf("expected error invalid_client, got %v", resp["error"])
	}
}

func TestTokenClientCredentialsUnknownClient(t *testing.T) {
	env := setupTokenEnv(t)

	req := makeTokenRequest("client_credentials", basicAuth("unknown-client", "some-secret"), nil)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_client" {
		t.Errorf("expected error invalid_client, got %v", resp["error"])
	}
}

func TestTokenExchangeValid(t *testing.T) {
	env := setupTokenEnv(t)

	subjectClaims := map[string]any{
		"sub":          "external-user",
		"caas_user_id": "ext-user-456",
		"caas_org_id":  env.tenantID,
		"user_roles":   []string{"viewer", "editor"},
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
	}
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest(
		"urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret),
		extra,
	)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	accessToken, ok := resp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatal("expected non-empty access_token")
	}
	if resp["token_type"] != "Bearer" {
		t.Errorf("expected token_type Bearer, got %v", resp["token_type"])
	}
	if resp["issued_token_type"] != "urn:ietf:params:oauth:token-type:jwt" {
		t.Errorf("expected issued_token_type, got %v", resp["issued_token_type"])
	}

	// Verify the OBO token claims.
	parsed, err := auth.Parse(accessToken)
	if err != nil {
		t.Fatalf("failed to parse OBO token: %v", err)
	}
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &env.signingKey.PublicKey); err != nil {
		t.Fatalf("OBO token signature verification failed: %v", err)
	}

	// OBO sub = subject token's sub (the user), not the M2M client.
	if parsed.Claims["sub"] != "external-user" {
		t.Errorf("expected sub %q, got %v", "external-user", parsed.Claims["sub"])
	}
	// caas_user_id also set to subject's sub for validator compatibility.
	if parsed.Claims["caas_user_id"] != "external-user" {
		t.Errorf("expected caas_user_id %q, got %v", "external-user", parsed.Claims["caas_user_id"])
	}
	if parsed.Claims["caas_org_id"] != env.tenantID {
		t.Errorf("expected caas_org_id %q, got %v", env.tenantID, parsed.Claims["caas_org_id"])
	}

	// Check actor claim.
	act, ok := parsed.Claims["act"].(map[string]any)
	if !ok {
		t.Fatal("expected act claim to be a map")
	}
	if act["sub"] != env.clientID {
		t.Errorf("expected act.sub %q, got %v", env.clientID, act["sub"])
	}
}

func TestTokenExchangeExpiredSubject(t *testing.T) {
	env := setupTokenEnv(t)

	subjectClaims := map[string]any{
		"sub":          "external-user",
		"caas_user_id": "ext-user-456",
		"caas_org_id":  env.tenantID,
		"user_roles":   []string{"viewer"},
		"exp":          float64(time.Now().Add(-time.Hour).Unix()), // expired
		"iat":          float64(time.Now().Add(-2 * time.Hour).Unix()),
	}
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest(
		"urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret),
		extra,
	)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_grant" {
		t.Errorf("expected error invalid_grant, got %v", resp["error"])
	}
}

func TestTokenExchangeTenantMismatch(t *testing.T) {
	env := setupTokenEnv(t)

	subjectClaims := map[string]any{
		"sub":          "external-user",
		"caas_user_id": "ext-user-456",
		"caas_org_id":  "different-tenant", // does not match M2M client's tenant
		"user_roles":   []string{"viewer"},
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
	}
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest(
		"urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret),
		extra,
	)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "access_denied" {
		t.Errorf("expected error access_denied, got %v", resp["error"])
	}
	if resp["error_description"] != "tenant mismatch" {
		t.Errorf("expected error_description 'tenant mismatch', got %v", resp["error_description"])
	}
}

func TestTokenExchangeUnknownTrustedKeyKID(t *testing.T) {
	env := setupTokenEnv(t)

	// Sign with a key whose KID is not registered in the trusted key store.
	unknownKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate unknown key: %v", err)
	}

	subjectClaims := map[string]any{
		"sub":          "external-user",
		"caas_user_id": "ext-user-456",
		"caas_org_id":  env.tenantID,
		"user_roles":   []string{"viewer"},
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
	}
	subjectToken := signSubjectToken(t, unknownKey, "unknown-kid", subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest(
		"urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret),
		extra,
	)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_grant" {
		t.Errorf("expected error invalid_grant, got %v", resp["error"])
	}
}

func TestTokenExchangeEmptySubClaim(t *testing.T) {
	env := setupTokenEnv(t)

	// Subject token with empty sub claim should be rejected.
	subjectClaims := map[string]any{
		"sub":          "", // empty sub
		"caas_user_id": "ext-user-456",
		"caas_org_id":  env.tenantID,
		"user_roles":   []string{"viewer"},
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
	}
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest(
		"urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret),
		extra,
	)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty sub, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_grant" {
		t.Errorf("expected error invalid_grant, got %v", resp["error"])
	}
}

func TestTokenExchangeMissingSubClaim(t *testing.T) {
	env := setupTokenEnv(t)

	// Subject token with no sub claim at all.
	subjectClaims := map[string]any{
		"caas_user_id": "ext-user-456",
		"caas_org_id":  env.tenantID,
		"user_roles":   []string{"viewer"},
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
	}
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest(
		"urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret),
		extra,
	)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing sub, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_grant" {
		t.Errorf("expected error invalid_grant, got %v", resp["error"])
	}
}

func TestTokenUnsupportedGrantType(t *testing.T) {
	env := setupTokenEnv(t)

	req := makeTokenRequest("authorization_code", basicAuth(env.clientID, env.clientSecret), nil)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "unsupported_grant_type" {
		t.Errorf("expected error unsupported_grant_type, got %v", resp["error"])
	}
}

func TestTokenExchangeInactiveTrustedKey(t *testing.T) {
	env := setupTokenEnv(t)

	// Invalidate the trusted key.
	if err := env.trustedKeyStore.Invalidate(env.trustedKID); err != nil {
		t.Fatalf("failed to invalidate trusted key: %v", err)
	}

	subjectClaims := map[string]any{
		"sub":          "external-user",
		"iss":          "external-idp",
		"caas_user_id": "user-456",
		"caas_org_id":  env.tenantID,
		"user_roles":   []string{"editor"},
		"exp":          float64(time.Now().Add(time.Hour).Unix()),
		"iat":          float64(time.Now().Unix()),
	}
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, subjectClaims)

	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")

	req := makeTokenRequest("urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret), extra)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for inactive trusted key, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_grant" {
		t.Errorf("expected error invalid_grant, got %v", resp["error"])
	}
}
