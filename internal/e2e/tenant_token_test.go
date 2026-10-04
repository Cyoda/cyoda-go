package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// tokenAnswer is what a token request got back.
type tokenAnswer struct {
	status      int
	oauthErr    string
	description string
	wwwAuth     string
	raw         []byte
	elapsed     time.Duration
}

// tokenStatus posts client_credentials to path on baseURL.
func tokenStatus(t *testing.T, baseURL, path, user, pass string) tokenAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(e2eCtx(t), http.MethodPost, baseURL+path, strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &body)
	return tokenAnswer{resp.StatusCode, body.Error, body.Description, resp.Header.Get("WWW-Authenticate"), raw, elapsed}
}

// wantInvalidTenant asserts the refusal the tenant route group writes.
func wantInvalidTenant(t *testing.T, what string, a tokenAnswer) {
	t.Helper()
	if a.status != http.StatusBadRequest || a.oauthErr != "invalid_request" || a.description != "invalid tenant" {
		t.Errorf("%s: %d %q %q: %s", what, a.status, a.oauthErr, a.description, withheld(a.status, a.raw))
	}
}

func TestTenantToken_UnknownTenantClientAndWrongSecretAreTheSame401(t *testing.T) {
	id, secret := createClient(t, false, false) // suite tenant
	cases := []struct{ name, tenant, user, pass string }{
		{"unknown tenant", "no-such-tenant", id, secret},
		{"unknown client", suiteTenant, "no-such-client", secret},
		{"wrong secret", suiteTenant, id, "wrong"},
	}
	var first tokenAnswer
	for i, c := range cases {
		a := tokenStatus(t, serverURL, "/api/tenants/"+c.tenant+"/oauth/token", c.user, c.pass)
		if a.status != http.StatusUnauthorized || a.oauthErr != "invalid_client" || a.elapsed < 500*time.Millisecond {
			t.Errorf("%s: %d %q after %v: %s", c.name, a.status, a.oauthErr, a.elapsed, withheld(a.status, a.raw))
		}
		if i == 0 {
			first = a
			continue
		}
		if a.status != first.status || a.oauthErr != first.oauthErr || a.description != first.description || a.wwwAuth != first.wwwAuth {
			t.Errorf("%s differs from %s: (%d %q %q %q) vs (%d %q %q %q)", c.name, cases[0].name,
				a.status, a.oauthErr, a.description, a.wwwAuth,
				first.status, first.oauthErr, first.description, first.wwwAuth)
		}
	}
}

func TestTenantToken_GroupRefusals(t *testing.T) {
	id, secret := createClient(t, false, false)
	for _, p := range []string{
		"/api/tenants/SYSTEM/oauth/token",
		"/api/tenants/system/oauth/token",
		"/api/tenants/-x/oauth/token",
		"/api/tenants/%74est-tenant/oauth/token",
		"/api/%74enants/test-tenant/oauth/token",
		"/api/tenants/test-tenant/oauth/%74oken",
	} {
		wantInvalidTenant(t, p, tokenStatus(t, serverURL, p, id, secret))
	}
}

// The two cases the OpenAPI validator would reject run on a stack without it.
func TestTenantToken_EncodedSlashInTenant(t *testing.T) {
	h := newCalloutHarness(t, nil)
	code, raw := h.postClient(t, h.token(t))
	if code != http.StatusOK {
		t.Fatalf("create client: %d %s", code, withheld(code, raw))
	}
	c := decodeCredential(t, "create client", raw)
	deleteClientAtCleanup(t, h.baseURL, c.id, func() string { return h.token(t) })

	wantInvalidTenant(t, "encoded slash", tokenStatus(t, h.baseURL, "/api/tenants/a%2Fb/oauth/token", c.id, c.secret))
}

func TestTenantToken_OldPathIsGone(t *testing.T) {
	h := newCalloutHarness(t, nil)
	code, raw := h.postClient(t, h.token(t))
	if code != http.StatusOK {
		t.Fatalf("create client: %d %s", code, withheld(code, raw))
	}
	c := decodeCredential(t, "create client", raw)
	deleteClientAtCleanup(t, h.baseURL, c.id, func() string { return h.token(t) })

	a := tokenStatus(t, h.baseURL, "/api/oauth/token", c.id, c.secret)
	if a.status != http.StatusUnauthorized || strings.Contains(string(a.raw), "access_token") {
		t.Errorf("the old token path answered %d (body withheld)", a.status)
	}
}

func TestTenantToken_RouteWordsAsTenants(t *testing.T) {
	for _, tenant := range []string{"clients", "oauth", "model", "tenants"} {
		t.Run(tenant, func(t *testing.T) {
			id, secret := createM2MClient(t, tenant, "admin", true)
			tok := getTokenIn(t, tenant, id, secret)
			if !clientIDsOn(t, serverURL, tok)[id] {
				t.Errorf("GET /clients as tenant %q does not list client %s", tenant, id)
			}
		})
	}
}
