package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// testTokenEnv holds shared test fixtures for token endpoint tests.
type testTokenEnv struct {
	keyStore        *auth.KVKeyStore
	trustedKeyStore *auth.KVTrustedKeyStore
	m2mStore        *auth.KVM2MClientStore
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

	// The token endpoint signs with the bootstrap key of the "client"
	// audience.
	signingKey := newBootstrap(t)
	keyStore := newTestKeyStore(t, signingKey)
	trustedKeyStore := newTestTrustedStore(t)
	m2mStore := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0)

	// Generate trusted external key (simulates an external IdP).
	trustedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate trusted key: %v", err)
	}
	trustedKID := "trusted-kid-1"
	// Create M2M client.
	tenantID := "tenant-abc"
	err = trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID:       trustedKID,
		TenantID:  spi.TenantID(tenantID),
		PublicKey: &trustedKey.PublicKey,
		Audience:  "cyoda-go",
		Active:    true,
		ValidFrom: time.Now().Add(-time.Hour),
	}, auth.RotateOptions{})
	if err != nil {
		t.Fatalf("failed to register trusted key: %v", err)
	}
	clientID := "TESTM2MCLIENT"
	clientSecret, err := m2mStore.Create(systemCtx(), spi.TenantID(tenantID), clientID, "user-123", []string{"admin", "reader"})
	if err != nil {
		t.Fatalf("failed to create M2M client: %v", err)
	}

	handler := auth.NewTokenHandler(keyStore, trustedKeyStore, m2mStore, "cyoda", "", 3600)

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

// setupTokenEnvWithAudience is setupTokenEnv with a configured audience.
func setupTokenEnvWithAudience(t *testing.T, audience string) *testTokenEnv {
	t.Helper()
	env := setupTokenEnv(t)
	env.handler = auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, env.m2mStore, "cyoda", audience, 3600)
	return env
}

func TestTokenEndpoint_ClientCredentialsCarriesConfiguredAudience(t *testing.T) {
	env := setupTokenEnvWithAudience(t, "cyoda-api")
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	tok, _ := decodeResponse(t, rr)["access_token"].(string)
	p, err := auth.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Claims["aud"] != "cyoda-api" {
		t.Fatalf("aud = %v, want cyoda-api", p.Claims["aud"])
	}
}

func TestTokenEndpoint_NoAudienceConfiguredOmitsAud(t *testing.T) {
	env := setupTokenEnv(t)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
	tok, _ := decodeResponse(t, rr)["access_token"].(string)
	p, err := auth.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := p.Claims["aud"]; present {
		t.Fatalf("aud present without a configured audience: %v", p.Claims["aud"])
	}
}

// A store failure is the server failing, never the client's credentials
// being wrong: 500 server_error with a ticket, not 401 invalid_client.
func TestTokenEndpoint_StoreFailureIs500NotInvalidClient(t *testing.T) {
	env := setupTokenEnv(t)
	h := auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, auth.NewKVM2MClientStore(brokenKV{}, 0), "cyoda", "", 3600)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
	if rr.Code != http.StatusInternalServerError || decodeResponse(t, rr)["error"] != "server_error" {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

// An id outside the client-id grammar is 401 invalid_client and never
// reaches the store: the store here fails every call, so a read would
// answer 500.
func TestTokenEndpoint_MalformedClientIDIs401(t *testing.T) {
	env := setupTokenEnv(t)
	h := auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, auth.NewKVM2MClientStore(brokenKV{}, 0), "cyoda", "", 3600)
	for _, raw := range []string{"a%00b", "%FF", strings.Repeat("A", 101), "a%3Ab"} {
		req := makeTokenRequest("client_credentials", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw+":x")), nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || decodeResponse(t, rr)["error"] != "invalid_client" {
			t.Fatalf("%q: %d %s", raw, rr.Code, rr.Body.String())
		}
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
	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)
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
	env := setupTokenEnvWithAudience(t, "cyoda-api")

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
	if parsed.Claims["aud"] != "cyoda-api" {
		t.Errorf("expected aud %q, got %v", "cyoda-api", parsed.Claims["aud"])
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

// A trusted key belongs to the tenant that registered it. A subject token
// signed with another tenant's key is refused, even when its caas_org_id names
// the exchanging client's tenant — the claim is written by the key holder, so
// it cannot be what binds the key to a tenant.
func TestTokenExchangeKeyFromAnotherTenant(t *testing.T) {
	env := setupTokenEnv(t)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const otherKID = "other-tenant-kid"
	if err := env.trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID:       otherKID,
		TenantID:  spi.TenantID("tenant-other"),
		PublicKey: &otherKey.PublicKey,
		Audience:  "cyoda-go",
		Active:    true,
		ValidFrom: time.Now().Add(-time.Hour),
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("register other tenant's key: %v", err)
	}

	subjectToken := signSubjectToken(t, otherKey, otherKID, map[string]any{
		"sub":         "ext-user-1",
		"caas_org_id": env.tenantID,
		"user_roles":  []string{"ROLE_ADMIN"},
		"exp":         float64(time.Now().Add(time.Hour).Unix()),
		"iat":         float64(time.Now().Unix()),
	})
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
		t.Fatalf("expected 400 for another tenant's key, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp["error"] != "invalid_grant" || resp["error_description"] != "unknown trusted key" {
		t.Errorf("got %v / %v, want invalid_grant / unknown trusted key", resp["error"], resp["error_description"])
	}
}

// The exchanged token carries the subject's sub as its user id, so a sub the
// server would reject on every later request is rejected here, at the grant,
// rather than minted into a token that can never be used. The response names
// the reason and never repeats the value.
func TestTokenExchangeInvalidSubClaim(t *testing.T) {
	env := setupTokenEnv(t)

	for name, sub := range map[string]string{
		"newline":  "ext\nuser",
		"nul":      "ext\x00user",
		"too-long": strings.Repeat("u", 256),
		"reserved": "oidc:11111111-2222-3333-4444-555555555555:alice",
	} {
		t.Run(name, func(t *testing.T) {
			subjectClaims := map[string]any{
				"sub":         sub,
				"caas_org_id": env.tenantID,
				"user_roles":  []string{"viewer"},
				"exp":         float64(time.Now().Add(time.Hour).Unix()),
				"iat":         float64(time.Now().Unix()),
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
				t.Fatalf("expected 400 for an invalid sub, got %d: %s", rr.Code, rr.Body.String())
			}
			resp := decodeResponse(t, rr)
			if resp["error"] != "invalid_grant" {
				t.Errorf("expected error invalid_grant, got %v", resp["error"])
			}
			if strings.Contains(rr.Body.String(), sub) {
				t.Errorf("response echoes the rejected sub: %s", rr.Body.String())
			}
		})
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

func TestTokenHandler_NonPost_405MethodNotAllowed(t *testing.T) {
	env := setupTokenEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/oauth/token", nil)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp["error"] != "method_not_allowed" {
		t.Errorf("expected error method_not_allowed, got %v", resp["error"])
	}
}

// Invalidating with a grace period keeps the key verifying until the grace
// period ends — the contract of the invalidate operation, and what a rotation
// with invalidatePrevious relies on to avoid an outage.
func TestTokenExchangeKeyInGracePeriod(t *testing.T) {
	env := setupTokenEnv(t)
	if err := env.trustedKeyStore.Invalidate(context.Background(), spi.TenantID(env.tenantID), env.trustedKID, 3600); err != nil {
		t.Fatalf("failed to invalidate trusted key: %v", err)
	}

	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, map[string]any{
		"sub":         "ext-user-1",
		"caas_org_id": env.tenantID,
		"user_roles":  []string{"editor"},
		"exp":         float64(time.Now().Add(time.Hour).Unix()),
		"iat":         float64(time.Now().Unix()),
	})
	extra := url.Values{}
	extra.Set("subject_token", subjectToken)
	extra.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
	req := makeTokenRequest("urn:ietf:params:oauth:grant-type:token-exchange",
		basicAuth(env.clientID, env.clientSecret), extra)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a key in its grace period, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTokenExchangeInactiveTrustedKey(t *testing.T) {
	env := setupTokenEnv(t)

	// Invalidate the trusted key.
	if err := env.trustedKeyStore.Invalidate(context.Background(), spi.TenantID(env.tenantID), env.trustedKID, 0); err != nil {
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

// failingKeyStore is an auth.KeyStore whose Signer always fails; the other
// methods are not exercised by this test and return the same error.
type failingKeyStore struct{ err error }

func (f failingKeyStore) Signer(string) (*auth.KeyPair, auth.Signer, error) { return nil, nil, f.err }
func (f failingKeyStore) Current(string) (*auth.KeyPair, error)             { return nil, f.err }
func (f failingKeyStore) VerificationKey(string) (*rsa.PublicKey, error)    { return nil, f.err }
func (f failingKeyStore) Published() ([]*auth.KeyPair, error)               { return nil, f.err }
func (f failingKeyStore) Issue(context.Context, auth.IssueRequest) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Invalidate(context.Context, string, int64) error { return f.err }
func (f failingKeyStore) Reactivate(context.Context, string, time.Time, time.Time) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Delete(context.Context, string) error { return f.err }

// TestTokenEndpoint_ServerErrorCarriesTicket pins Gate 3 for this endpoint:
// every 5xx carries a generic message plus a ticket UUID and no internals.
// The OAuth2 body shape has no dedicated field, so the ticket rides in
// error_description, which the schema already declares as a string.
//
// A ticket is only worth minting if an operator can find it: the same UUID
// must appear in the log record that carries the underlying cause, which is
// what ties a caller's report to the failure. The log is captured here rather
// than asserted through a running stack, because inducing a key-store failure
// over HTTP would need a production seam this project forbids.
func TestTokenEndpoint_ServerErrorCarriesTicket(t *testing.T) {
	env := setupTokenEnv(t)

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// A key store whose Signer always fails drives the server_error path.
	// The trusted-key store, M2M store and client credentials come from the
	// shared env so the request authenticates normally before hitting the
	// failing key store.
	cause := errors.New("hsm unreachable at 10.0.0.5:8443")
	h := auth.NewTokenHandler(
		failingKeyStore{err: cause},
		env.trustedKeyStore,
		env.m2mStore,
		"cyoda-test",
		"",
		3600,
	)

	req := makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp["error"] != "server_error" {
		t.Errorf("error = %v, want server_error", resp["error"])
	}
	desc, _ := resp["error_description"].(string)
	if !strings.HasPrefix(desc, "server_error [ticket: ") {
		t.Fatalf("error_description = %q, want a ticket", desc)
	}
	ticket := strings.TrimSuffix(strings.TrimPrefix(desc, "server_error [ticket: "), "]")
	if _, err := uuid.Parse(ticket); err != nil {
		t.Errorf("ticket %q is not a uuid: %v", ticket, err)
	}
	if strings.Contains(rr.Body.String(), "hsm") || strings.Contains(rr.Body.String(), "10.0.0.5") {
		t.Errorf("response leaks the underlying cause: %s", rr.Body.String())
	}

	// The same ticket reaches the log, with the cause the response withheld.
	logged := logBuf.String()
	if !strings.Contains(logged, ticket) {
		t.Errorf("ticket %q never reaches the log; an operator cannot correlate the caller's report:\n%s",
			ticket, logged)
	}
	if !strings.Contains(logged, "hsm unreachable") {
		t.Errorf("the underlying cause is missing from the log record:\n%s", logged)
	}
}

// An error response from the token endpoint is not cacheable either, so
// every /oauth/token response carries the same headers.
func TestTokenEndpoint_ErrorResponsesAreNotCacheable(t *testing.T) {
	env := setupTokenEnv(t)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(env.clientID, "wrong"), nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
}

// RFC 6749 §5.1: a response carrying a token has Cache-Control: no-store and
// Pragma: no-cache, on both grants.
func TestTokenEndpoint_TokenResponsesAreNotCacheable(t *testing.T) {
	env := setupTokenEnv(t)
	subjectToken := signSubjectToken(t, env.trustedKey, env.trustedKID, map[string]any{
		"sub":         "external-user",
		"caas_org_id": env.tenantID,
		"user_roles":  []string{"viewer"},
		"exp":         float64(time.Now().Add(time.Hour).Unix()),
		"iat":         float64(time.Now().Unix()),
	})
	exchange := url.Values{}
	exchange.Set("subject_token", subjectToken)
	exchange.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
	for name, req := range map[string]*http.Request{
		"client_credentials": makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil),
		"token_exchange":     makeTokenRequest("urn:ietf:params:oauth:grant-type:token-exchange", basicAuth(env.clientID, env.clientSecret), exchange),
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			env.handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := rr.Header().Get("Pragma"); got != "no-cache" {
				t.Errorf("Pragma = %q, want no-cache", got)
			}
		})
	}
}
