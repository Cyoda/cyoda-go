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
	"time"

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
	data := url.Values{
		"grant_type": {"client_credentials"},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", serverURL+"/api/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
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

// authRequestRaw creates an authenticated HTTP request.
func authRequestRaw(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	token, err := getTokenRaw(ctx, "testclient", "testsecret")
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
