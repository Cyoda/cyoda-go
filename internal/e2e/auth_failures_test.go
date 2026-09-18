package e2e_test

// Authentication-failure coverage through the full HTTP stack. The rejection
// logic itself is unit-tested in internal/auth; what these tests pin is that
// every authenticated route is actually wrapped in the auth middleware and
// that the rejection reaches the client as the uniform RFC 9457 problem
// detail — routing and wiring the unit tests cannot see.

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
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

// TestAuth_RejectionCarriesNoEnumerationSignal asserts that the 401 a
// protected route returns is identical no matter how the credential was
// malformed — absent, empty, unparseable, wrong scheme, or signed by an
// untrusted key. Any variation would let a caller probe which part of the
// credential the server got far enough to reject.
//
// The credential-validity oracle proper (valid client + wrong secret vs.
// unknown client) lives on the token endpoint, not here; it is covered by
// internal/auth/delegating_uniform_message_test.go and
// delegating_enumeration_test.go.
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

// mintTokenWithTenant signs a first-party token carrying an arbitrary
// caas_org_id, so a test can present a claim no legitimate client could
// obtain. The kid is the one app.NewAuthService derives from the signing key's
// public part (sha256(SPKI)[:16] hex).
func mintTokenWithTenant(t *testing.T, tenant string) string {
	t.Helper()
	pubDER, err := x509.MarshalPKIXPublicKey(&e2eSignKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	sum := sha256.Sum256(pubDER)
	kid := hex.EncodeToString(sum[:16])

	now := time.Now()
	tok, err := auth.Sign(map[string]any{
		"sub":          "e2e-tenant-probe",
		"iss":          e2eIssuer,
		"caas_user_id": "e2e-tenant-probe",
		"caas_org_id":  tenant,
		"user_roles":   []string{"ROLE_ADMIN"},
		"exp":          now.Add(time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}, e2eSignKey, kid)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

// TestAuth_TenantClaimOutsideGrammar_401 proves door 1 holds through the full
// HTTP stack, on a token this server itself would accept but for the claim.
// The rejection must be indistinguishable from any other bad token.
func TestAuth_TenantClaimOutsideGrammar_401(t *testing.T) {
	for name, tenant := range map[string]string{
		"traversal": "../victim",
		"dotdot":    "..",
		"slash":     "a/b",
		"colon":     "a:b",
		"newline":   "tenant\ninjected",
		"too-long":  strings.Repeat("x", 101),
	} {
		t.Run(name, func(t *testing.T) {
			resp := unauthRequest(t, http.MethodGet, "/api/entity/e2e-auth-probe/1",
				"Bearer "+mintTokenWithTenant(t, tenant))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d, want 401; body: %s", resp.StatusCode, raw)
			}
			assertUnauthorizedProblem(t, resp)

			raw, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(raw), "victim") || strings.Contains(string(raw), "injected") {
				t.Errorf("response echoes the rejected tenant: %s", raw)
			}
		})
	}
}

// TestAuth_AcceptedTenantShapesStillAuthenticate is the regression half: the
// grammar must not lock out a shape that works today. 401 here would be a
// production lockout; anything else means the token was accepted.
func TestAuth_AcceptedTenantShapesStillAuthenticate(t *testing.T) {
	for _, tenant := range []string{
		"SYSTEM",
		"default-tenant",
		"tenant-abc-123",
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
		"conformance-1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
	} {
		t.Run(tenant, func(t *testing.T) {
			resp := unauthRequest(t, http.MethodGet, "/api/model/",
				"Bearer "+mintTokenWithTenant(t, tenant))
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("tenant %q was rejected — this is a lockout; body: %s", tenant, raw)
			}
		})
	}
}
