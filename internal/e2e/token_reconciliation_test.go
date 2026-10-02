package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// postToken issues a POST to the shared server's /api/oauth/token with the
// given form values and optional HTTP Basic credentials. It does NOT use
// authRequest because the token endpoint does not require a pre-existing
// bearer token.
func postToken(t *testing.T, form url.Values, basicUser, basicPass string) *http.Response {
	t.Helper()
	return postTokenTo(t, serverURL, form, basicUser, basicPass)
}

// postTokenTo is postToken against the server at baseURL — the shared server
// or a harness stack. basicUser and basicPass are sent as given, so a caller
// can pass a form-urlencoded client id (the endpoint decodes both parts).
func postTokenTo(t *testing.T, baseURL string, form url.Values, basicUser, basicPass string) *http.Response {
	t.Helper()
	resp, err := postTokenRaw(e2eCtx(t), baseURL, form, basicUser, basicPass)
	if err != nil {
		t.Fatalf("postToken: %v", err)
	}
	return resp
}

// postTokenRaw is the one /api/oauth/token request every token helper
// sends: form as the body, basicUser and basicPass as HTTP Basic credentials
// when basicUser is set. It never touches *testing.T, so it is safe to call
// from a goroutine.
func postTokenRaw(ctx context.Context, baseURL string, form url.Values, basicUser, basicPass string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicUser != "" {
		req.SetBasicAuth(basicUser, basicPass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	return resp, nil
}

// assertOAuthError asserts the flat RFC-6749 error shape (application/json,
// {error, error_description}) — NOT ProblemDetail. The token endpoint
// deliberately keeps ErrorResponseDto for OAuth wire compatibility.
func assertOAuthError(t *testing.T, resp *http.Response, wantStatus int, wantErr string) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("assertOAuthError: status: got %d, want %d; body=%s", resp.StatusCode, wantStatus, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("assertOAuthError: content-type: got %q, want application/json (flat OAuth shape); body=%s", ct, raw)
	}
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("assertOAuthError: unmarshal: %v; body=%s", err, raw)
	}
	if e.Error != wantErr {
		t.Fatalf("assertOAuthError: error: got %q, want %q; body=%s", e.Error, wantErr, raw)
	}
	if e.ErrorDescription == "" {
		t.Fatalf("assertOAuthError: error_description empty (spec marks it required); body=%s", raw)
	}
}

// TestToken_ClientCredentials_Accepted verifies the primary M2M path:
// client_credentials grant with valid Basic credentials returns 200.
func TestToken_ClientCredentials_Accepted(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	id, secret := createClient(t, false, false)
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, id, secret)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("client_credentials: got %d, want 200; body=%s", resp.StatusCode, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("client_credentials: content-type: got %q, want application/json", ct)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("client_credentials: unmarshal: %v; body=%s", err, raw)
	}
	if tok.AccessToken == "" {
		t.Fatalf("client_credentials: empty access_token; body=%s", raw)
	}
	if tok.TokenType != "Bearer" {
		t.Fatalf("client_credentials: token_type: got %q, want Bearer; body=%s", tok.TokenType, raw)
	}
	if tok.ExpiresIn <= 0 {
		t.Fatalf("client_credentials: expires_in: got %d, want >0; body=%s", tok.ExpiresIn, raw)
	}
	// §4.2: the client is sub and caas_user_id, and cgen is its secret
	// generation, 1 for a client whose secret was never reset.
	claims := decodeJWTPayload(t, tok.AccessToken)
	if claims["sub"] != id || claims["caas_user_id"] != id || claims["cgen"] != float64(1) {
		t.Errorf("claims sub=%v caas_user_id=%v cgen=%v, want %s, %s, 1", claims["sub"], claims["caas_user_id"], claims["cgen"], id, id)
	}
}

// TestToken_BadGrantType_400UnsupportedGrantType verifies that an unknown
// grant_type returns 400 unsupported_grant_type.
func TestToken_BadGrantType_400UnsupportedGrantType(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	id, secret := createClient(t, false, false)
	resp := postToken(t, url.Values{"grant_type": {"password"}}, id, secret)
	assertOAuthError(t, resp, http.StatusBadRequest, "unsupported_grant_type")
}

// TestToken_BadClient_401InvalidClient verifies that wrong Basic credentials
// return 401 invalid_client.
func TestToken_BadClient_401InvalidClient(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	id, _ := createClient(t, false, false)
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, id, "wrongsecret")
	if got := resp.Header.Get("WWW-Authenticate"); got != "Basic" {
		t.Errorf("WWW-Authenticate = %q, want Basic", got)
	}
	assertOAuthError(t, resp, http.StatusUnauthorized, "invalid_client")
}

// NOTE: every token-exchange refusal is covered by
// TestToken_TokenExchange_Refusals in token_exchange_test.go.
//
// NOTE: 503 temporarily_unavailable and 500 server_error are store and
// signing failures a running backend cannot be made to produce on demand;
// they are covered by the unit tests in internal/auth/token_test.go.
