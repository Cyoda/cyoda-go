package e2e_test

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// The helpers below come in two shapes. The `t`-taking form (doAuth, readBody,
// …) aborts the test with t.Fatalf on transport failure and is the default for
// sequential test code. The `Raw` form takes a context.Context and returns an
// error instead; it is the only shape that may be used from a goroutine other
// than the one running the test, because Fatal/FailNow only stop the calling
// goroutine. The `t` form is a thin wrapper over the `Raw` form, so the request
// behaviour (auth, retry-on-retryable-409) is defined exactly once.
// internal/e2e/goroutinesafety enforces the split.

// httpResult is an HTTP outcome captured off the test goroutine, for assertion
// on the test goroutine after the concurrent phase has joined.
type httpResult struct {
	status int
	body   string
	err    error
}

// resultOf drains resp into an httpResult. It is written to consume the
// (*http.Response, error) pair returned by the Raw helpers directly:
//
//	res := resultOf(doAuthRaw(ctx, http.MethodPost, path, payload))
func resultOf(resp *http.Response, err error) httpResult {
	if err != nil {
		return httpResult{status: -1, err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpResult{status: resp.StatusCode, err: fmt.Errorf("read body: %w", err)}
	}
	return httpResult{status: resp.StatusCode, body: string(raw)}
}

// e2eCtx returns the request context for a test's HTTP calls.
//
// It attaches t via openapivalidator.WithTestT for continuity with the
// existing helpers, but note that this does NOT reach the validator: the
// suite talks to a real httptest TCP listener (see TestMain), and the
// middleware reads TestTFromContext(r.Context()) — the *server's* context,
// which is built fresh per connection. A client-side context value cannot
// cross the wire, so the validator's TestName is always "unknown" and its
// enforce-mode t.Errorf never fires. That is pre-existing; wiring it up
// needs the test identity carried in a header and resolved server-side.
//
// What the context is actually good for here is the standard thing —
// cancellation and deadlines — and giving the Raw helpers an idiomatic
// first parameter. Nothing about goroutine safety depends on it.
func e2eCtx(t *testing.T) context.Context {
	t.Helper()
	return openapivalidator.WithTestT(context.Background(), t)
}

// e2eNewRequest creates an http.Request bound to the test's context.
func e2eNewRequest(t *testing.T, method, urlStr string, body io.Reader) (*http.Request, error) {
	t.Helper()
	return http.NewRequestWithContext(e2eCtx(t), method, urlStr, body)
}

// getTokenRaw obtains a JWT token via client_credentials grant. The token
// endpoint uses HTTP Basic Auth for client authentication.
func getTokenRaw(ctx context.Context, clientID, clientSecret string) (string, error) {
	resp, err := postTokenRaw(ctx, serverURL, url.Values{"grant_type": {"client_credentials"}}, clientID, clientSecret)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request returned %d: %s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	token, ok := result["access_token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("no access_token in response: %v", result)
	}
	return token, nil
}

// getToken is the test-goroutine form of getTokenRaw.
func getToken(t *testing.T, clientID, clientSecret string) string {
	t.Helper()
	token, err := getTokenRaw(e2eCtx(t), clientID, clientSecret)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	return token
}

// signServiceToken signs a token in the shape of a client_credentials token
// (scopes → a service principal) with key, for the given issuer/audience/
// sub/tenant/userID/roles; an empty audience omits the aud claim, which a
// stack with CYODA_JWT_AUDIENCE set requires. Every "admin token for a stack"
// helper in this package (suiteTokenRaw, callbackHarness.fetchToken/
// adminTokenFor, adminTokenForTenant, newStandaloneApp, bootstrapToken) is a
// thin wrapper over this one signing call. It never touches *testing.T.
func signServiceToken(key *rsa.PrivateKey, issuer, audience, sub, tenant, userID string, roles []string) (string, error) {
	kid, err := auth.DeriveKID(&key.PublicKey)
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := map[string]any{
		"sub":          sub,
		"iss":          issuer,
		"caas_user_id": userID,
		"caas_org_id":  tenant,
		"scopes":       roles,
		"caas_tier":    "unlimited",
		"exp":          now.Add(time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}
	if audience != "" {
		claims["aud"] = audience
	}
	return auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)
}

// suiteTokenRaw signs an admin token for the shared server in the shape of a
// client_credentials token (scopes → a service principal), so attribution
// assertions are unchanged. It never touches *testing.T.
func suiteTokenRaw() (string, error) {
	return signServiceToken(e2eSignKey, e2eIssuer, "", "suite-admin", "test-tenant", "test-admin", []string{"ROLE_ADMIN", "ROLE_M2M"})
}

// suiteToken is the test-goroutine form of suiteTokenRaw.
func suiteToken(t *testing.T) string {
	t.Helper()
	tok, err := suiteTokenRaw()
	if err != nil {
		t.Fatalf("sign suite token: %v", err)
	}
	return tok
}

// deleteClientAtCleanup registers a t.Cleanup that deletes the M2M client id
// through DELETE {baseURL}/api/clients/{id}, authenticated with the token
// bearer returns when the cleanup runs. The request runs on a context of its
// own (t.Context() is cancelled before cleanups run). A 404 is accepted: the
// test may have deleted the client itself.
func deleteClientAtCleanup(t *testing.T, baseURL, id string, bearer func() string) {
	t.Helper()
	t.Cleanup(func() {
		req, err := http.NewRequestWithContext(e2eCtx(t), http.MethodDelete, baseURL+"/api/clients/"+url.PathEscape(id), nil)
		if err != nil {
			t.Errorf("cleanup: delete client %s: %v", id, err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+bearer())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("cleanup: delete client %s: %v", id, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("cleanup: delete client %s: %d %s", id, resp.StatusCode, b)
		}
	})
}

// createClient creates an M2M client in the suite tenant through POST
// /clients and returns its id and secret (never log the secret). The client
// is deleted when the test ends.
func createClient(t *testing.T, withAdminRole bool) (string, string) {
	t.Helper()
	path := "/api/clients"
	if withAdminRole {
		path += "?withAdminRole=true"
	}
	resp := doAuth(t, http.MethodPost, path, "")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create client: %d %s", resp.StatusCode, raw)
	}
	cred := decodeCredential(t, "create client", raw)
	deleteClientAtCleanup(t, serverURL, cred.id, func() string { return suiteToken(t) })
	return cred.id, cred.secret
}

// decodeCredential decodes the client_id and client_secret of a 200 answer
// from POST /clients or PUT /clients/{id}/secret; what names the call in a
// failure message. raw holds a secret, so it is never printed.
func decodeCredential(t *testing.T, what string, raw []byte) m2mCredential {
	t.Helper()
	var cred struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil || cred.ID == "" || cred.Secret == "" {
		t.Fatalf("%s: no credentials in response (%v)", what, err)
	}
	return m2mCredential{id: cred.ID, secret: cred.Secret}
}

// withheld is raw for a failure message, unless status is 200: a 200 from
// POST /clients or PUT /clients/{id}/secret carries a secret.
func withheld(status int, raw []byte) string {
	if status == http.StatusOK {
		return "(credentials withheld)"
	}
	return string(raw)
}

// authRequestRaw creates an authenticated HTTP request.
func authRequestRaw(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := suiteTokenRaw()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, serverURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// authRequest is the test-goroutine form of authRequestRaw.
func authRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := authRequestRaw(e2eCtx(t), method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return req
}

// doAuthRaw performs an authenticated HTTP request and returns the response.
// On 409 Conflict with properties.retryable=true (SERIALIZABLE 40001/40P01
// aborts, classified by the server), retries up to 5 times with a short
// backoff. Non-retryable 409s (business-logic conflicts) are returned to
// the caller on the first response.
//
// It never touches *testing.T, so it is safe to call from a goroutine.
func doAuthRaw(ctx context.Context, method, path string, body string) (*http.Response, error) {
	const maxAttempts = 5
	var resp *http.Response
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := authRequestRaw(ctx, method, path, bodyReader)
		if err != nil {
			return nil, err
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s failed: %w", method, path, err)
		}
		if r.StatusCode != http.StatusConflict {
			return r, nil
		}
		// Peek the body without consuming it — caller still owns r.Body.
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if !isRetryableConflict(raw) {
			// Not safe to retry; return the response with a re-stuffed body
			// so the caller can read it normally.
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			return r, nil
		}
		resp = r
		resp.Body = io.NopCloser(strings.NewReader(string(raw)))
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
	}
	return resp, nil
}

// doAuth is the test-goroutine form of doAuthRaw.
func doAuth(t *testing.T, method, path string, body string) *http.Response {
	t.Helper()
	resp, err := doAuthRaw(e2eCtx(t), method, path, body)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return resp
}

// isRetryableConflict reports whether a 409 body advertises
// properties.retryable=true (the server's classified-serialization-abort
// signal). See e2e/parity/client for the shared implementation.
func isRetryableConflict(body []byte) bool {
	var problem struct {
		Properties struct {
			Retryable bool `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		return false
	}
	return problem.Properties.Retryable
}

// readBody reads and returns the response body as a string, closing it.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return string(body)
}

// queryDB executes a SQL query against the test database with tenant set.
func queryDB(t *testing.T, tenantID, sql string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	tx, err := dbPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	// NOTE: test-only — tenantID is a hardcoded constant, not user input. Do not use this pattern in production code.
	_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.current_tenant = '%s'", tenantID))
	if err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var count int
	err = tx.QueryRow(ctx, sql, args...).Scan(&count)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	return count
}
