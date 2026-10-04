package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// tokenStatus posts client_credentials to path on baseURL and returns the
// status, the OAuth error, the raw body and the elapsed time.
func tokenStatus(t *testing.T, baseURL, path, user, pass string) (int, string, []byte, time.Duration) {
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
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body.Error, raw, elapsed
}

func TestTenantToken_UnknownTenantClientAndWrongSecretAreTheSame401(t *testing.T) {
	id, secret := createClient(t, false, false) // suite tenant
	for name, c := range map[string]struct{ tenant, user, pass string }{
		"unknown tenant": {"no-such-tenant", id, secret},
		"unknown client": {suiteTenant, "no-such-client", secret},
		"wrong secret":   {suiteTenant, id, "wrong"},
	} {
		code, oauthErr, raw, d := tokenStatus(t, serverURL, "/api/tenants/"+c.tenant+"/oauth/token", c.user, c.pass)
		if code != http.StatusUnauthorized || oauthErr != "invalid_client" || d < 500*time.Millisecond {
			t.Errorf("%s: %d %q after %v: %s", name, code, oauthErr, d, withheld(code, raw))
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
		if code, oauthErr, raw, _ := tokenStatus(t, serverURL, p, id, secret); code != http.StatusBadRequest || oauthErr != "invalid_request" {
			t.Errorf("%s: %d %q: %s", p, code, oauthErr, withheld(code, raw))
		}
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

	status, oauthErr, body, _ := tokenStatus(t, h.baseURL, "/api/tenants/a%2Fb/oauth/token", c.id, c.secret)
	if status != http.StatusBadRequest || oauthErr != "invalid_request" {
		t.Errorf("encoded slash: %d %q: %s", status, oauthErr, withheld(status, body))
	}
}

func TestTenantToken_OldPathIsGone(t *testing.T) {
	h := newCalloutHarness(t, nil)
	code, raw := h.postClient(t, h.token(t))
	if code != http.StatusOK {
		t.Fatalf("create client: %d %s", code, withheld(code, raw))
	}
	c := decodeCredential(t, "create client", raw)
	deleteClientAtCleanup(t, h.baseURL, c.id, func() string { return h.token(t) })

	status, _, body, _ := tokenStatus(t, h.baseURL, "/api/oauth/token", c.id, c.secret)
	if status == http.StatusOK || strings.Contains(string(body), "access_token") {
		t.Errorf("the old token path answered %d (credentials withheld)", status)
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
