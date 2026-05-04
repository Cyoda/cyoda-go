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

// e2eNewRequest creates an http.Request with the test name attached to the
// request context via openapivalidator.WithTestT. The validator middleware
// uses the captured *testing.T to call t.Errorf in -run-filtered enforce
// mode (see openapivalidator/doc.go).
func e2eNewRequest(t *testing.T, method, urlStr string, body io.Reader) (*http.Request, error) {
	t.Helper()
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(openapivalidator.WithTestT(req.Context(), t))
	return req, nil
}

// getToken obtains a JWT token via client_credentials grant.
// The token endpoint uses HTTP Basic Auth for client authentication.
func getToken(t *testing.T, clientID, clientSecret string) string {
	t.Helper()
	data := url.Values{
		"grant_type": {"client_credentials"},
	}
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		t.Fatalf("failed to create token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("token request returned %d: %s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode token response: %v", err)
	}
	token, ok := result["access_token"].(string)
	if !ok || token == "" {
		t.Fatalf("no access_token in response: %v", result)
	}
	return token
}

// authRequest creates an authenticated HTTP request.
func authRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	token := getToken(t, "test-client", "test-secret")
	req, err := e2eNewRequest(t, method, serverURL+path, body)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// doAuth performs an authenticated HTTP request and returns the response.
// On 409 Conflict with properties.retryable=true (SERIALIZABLE 40001/40P01
// aborts, classified by the server), retries up to 5 times with a short
// backoff. Non-retryable 409s (business-logic conflicts) are returned to
// the caller on the first response.
func doAuth(t *testing.T, method, path string, body string) *http.Response {
	t.Helper()
	const maxAttempts = 5
	var resp *http.Response
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req := authRequest(t, method, path, bodyReader)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s failed: %v", method, path, err)
		}
		if r.StatusCode != http.StatusConflict {
			return r
		}
		// Peek the body without consuming it — caller still owns r.Body.
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if !isRetryableConflict(raw) {
			// Not safe to retry; return the response with a re-stuffed body
			// so the caller can read it normally.
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			return r
		}
		resp = r
		resp.Body = io.NopCloser(strings.NewReader(string(raw)))
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
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
