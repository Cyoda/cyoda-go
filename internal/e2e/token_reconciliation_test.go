package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// postToken issues a POST to /api/oauth/token with the given form values and
// optional HTTP Basic credentials. It does NOT use authRequest because the
// token endpoint does not require a pre-existing bearer token.
func postToken(t *testing.T, form url.Values, basicUser, basicPass string) *http.Response {
	t.Helper()
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("postToken: new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicUser != "" {
		req.SetBasicAuth(basicUser, basicPass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postToken: do: %v", err)
	}
	return resp
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
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, "testclient", "testsecret")
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
}

// TestToken_BadGrantType_400UnsupportedGrantType verifies that an unknown
// grant_type returns 400 unsupported_grant_type.
func TestToken_BadGrantType_400UnsupportedGrantType(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := postToken(t, url.Values{"grant_type": {"password"}}, "testclient", "testsecret")
	assertOAuthError(t, resp, http.StatusBadRequest, "unsupported_grant_type")
}

// TestToken_BadClient_401InvalidClient verifies that wrong Basic credentials
// return 401 invalid_client.
func TestToken_BadClient_401InvalidClient(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, "testclient", "wrongsecret")
	assertOAuthError(t, resp, http.StatusUnauthorized, "invalid_client")
}

// TestToken_TokenExchange_InvalidGrant_BadSubjectTokenType verifies that a
// token-exchange request with an unsupported subject_token_type returns 400
// invalid_grant. This exercises a trigger in handleTokenExchange without
// requiring private-key material (wrong type is rejected before verification).
func TestToken_TokenExchange_InvalidGrant_BadSubjectTokenType(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {"fake.token.here"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"}, // not jwt — rejected
	}
	resp := postToken(t, form, "testclient", "testsecret")
	assertOAuthError(t, resp, http.StatusBadRequest, "invalid_grant")
}

// TestToken_TokenExchange_InvalidGrant_MalformedToken verifies that a
// token-exchange request with a structurally invalid subject_token returns 400
// invalid_grant. The JWT parser rejects the non-JWT string before any key lookup.
func TestToken_TokenExchange_InvalidGrant_MalformedToken(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {"not-a-jwt"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
	}
	resp := postToken(t, form, "testclient", "testsecret")
	assertOAuthError(t, resp, http.StatusBadRequest, "invalid_grant")
}

// NOTE: access_denied (403 tenant mismatch) requires constructing a valid
// signed JWT whose caas_org_id differs from the authenticating M2M client's
// tenant. That requires private-key material for a registered trusted key.
// The E2E harness does not retain private keys after registration (see
// oauth_keys_test.go NOTE at line ~570). This case is waived at E2E level;
// the unit-level invariant is asserted in internal/auth tests.
//
// NOTE: server_error (500) is an internal-fault path only. No producing test
// is provided; the enum addition in ErrorResponseDto is sufficient per the
// task brief.
