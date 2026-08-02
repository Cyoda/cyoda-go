package e2e_test

// Authentication-failure coverage through the full HTTP stack. The rejection
// logic itself is unit-tested in internal/auth; what these tests pin is that
// every authenticated route is actually wrapped in the auth middleware and
// that the rejection reaches the client as the uniform RFC 9457 problem
// detail — routing and wiring the unit tests cannot see.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// unauthRequest issues a request to path with the given Authorization header
// verbatim ("" omits the header entirely).
func unauthRequest(t *testing.T, method, path, authHeader string) *http.Response {
	t.Helper()
	req, err := e2eNewRequest(t, method, serverURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// A syntactically valid JWT signed by a key the server does not trust.
const untrustedJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
	"eyJzdWIiOiJhdHRhY2tlciIsImlzcyI6ImN5b2RhLXRlc3QiLCJleHAiOjQxMDI0NDQ4MDB9." +
	"ZmFrZXNpZ25hdHVyZWZha2VzaWduYXR1cmVmYWtlc2ln"

// TestAuth_MissingOrInvalidCredentials_401 asserts that every authenticated
// route rejects absent and malformed credentials with 401 UNAUTHORIZED.
func TestAuth_MissingOrInvalidCredentials_401(t *testing.T) {
	routes := []struct {
		name   string
		method string
		path   string
	}{
		{"entity-list", http.MethodGet, "/api/entity/e2e-auth-probe/1"},
		{"entity-create", http.MethodPost, "/api/entity/JSON/e2e-auth-probe/1"},
		{"admin-log-level", http.MethodGet, "/api/admin/log-level"},
		{"clients-list", http.MethodGet, "/api/clients"},
		{"model-export", http.MethodGet, "/api/model/export/SIMPLE_VIEW/e2e-auth-probe/1"},
	}
	credentials := []struct {
		name   string
		header string
	}{
		{"no-header", ""},
		{"empty-bearer", "Bearer "},
		{"garbage-bearer", "Bearer not-a-jwt"},
		{"wrong-scheme", "Basic dGVzdGNsaWVudDp0ZXN0c2VjcmV0"},
		{"untrusted-signature", "Bearer " + untrustedJWT},
	}

	for _, route := range routes {
		for _, cred := range credentials {
			t.Run(route.name+"/"+cred.name, func(t *testing.T) {
				resp := unauthRequest(t, route.method, route.path, cred.header)
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					raw, _ := io.ReadAll(resp.Body)
					t.Fatalf("status=%d, want 401; body: %s", resp.StatusCode, raw)
				}
				assertUnauthorizedProblem(t, resp)
			})
		}
	}
}

// assertUnauthorizedProblem checks the RFC 9457 shape of a 401 body.
func assertUnauthorizedProblem(t *testing.T, resp *http.Response) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("content-type=%q, want application/problem+json", ct)
	}
	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pd); err != nil {
		t.Fatalf("decode problem detail: %v", err)
	}
	if pd.Status != http.StatusUnauthorized {
		t.Errorf("problem.status=%d, want 401", pd.Status)
	}
	if got := pd.Properties["errorCode"]; got != "UNAUTHORIZED" {
		t.Errorf("errorCode=%v, want UNAUTHORIZED", got)
	}
}

// TestAuth_RejectionCarriesNoEnumerationSignal asserts that a 401 body is
// byte-identical no matter why authentication failed. A caller must not be
// able to distinguish "no such client", "wrong secret", "expired token" or
// "untrusted signer" from the response — that difference is an account
// enumeration oracle.
func TestAuth_RejectionCarriesNoEnumerationSignal(t *testing.T) {
	const path = "/api/entity/e2e-auth-probe/1"
	headers := []string{
		"",
		"Bearer ",
		"Bearer not-a-jwt",
		"Basic dGVzdGNsaWVudDp0ZXN0c2VjcmV0",
		"Bearer " + untrustedJWT,
	}

	var first string
	for i, header := range headers {
		resp := unauthRequest(t, http.MethodGet, path, header)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("header %q: status=%d, want 401", header, resp.StatusCode)
		}
		// The ticket UUID (if any) is per-request; compare the stable detail.
		var pd struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(body), &pd); err != nil {
			t.Fatalf("header %q: decode: %v", header, err)
		}
		got := fmt.Sprintf("%s|%s", pd.Title, pd.Detail)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("401 body differs by failure reason — enumeration signal.\n header %q: %q\n baseline: %q",
				header, got, first)
		}
	}
}

// TestAuth_ValidCredentialsStillPass guards against the 401 tests above
// passing for the wrong reason (e.g. a route that 401s unconditionally).
func TestAuth_ValidCredentialsStillPass(t *testing.T) {
	resp := doAuth(t, http.MethodGet, "/api/admin/log-level", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /api/admin/log-level: status=%d, want 200; body: %s", resp.StatusCode, body)
	}
}
