package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// registerOIDCProvider POSTs a minimal provider under the UUID tenant used by
// the OIDC lifecycle tests, driven by an admin token for that tenant.
// name is used as a unique suffix in the wellKnownConfigUri so two calls
// with the same name register the same URI (triggering the duplicate check).
func registerOIDCProvider(t *testing.T, token, name string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"wellKnownConfigUri": "http://oidc.example.test/" + name + "/.well-known/openid-configuration",
	})
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/oidc/providers", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestOIDC_RegisterDuplicate_Returns409(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	cid, secret := createM2MClient(t, oidcTenantUUID, "dup-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)

	first := registerOIDCProvider(t, token, "dupprov")
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first register: got %d, want 200", first.StatusCode)
	}

	dup := registerOIDCProvider(t, token, "dupprov")
	defer dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(dup.Body)
		t.Fatalf("duplicate register: got %d, want 409; body=%s", dup.StatusCode, raw)
	}
	// The machine code lives under properties.errorCode (ProblemDetail).
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	raw, _ := io.ReadAll(dup.Body)
	_ = json.Unmarshal(raw, &pd)
	if got := fmt.Sprintf("%v", pd.Properties["errorCode"]); got != "OIDC_PROVIDER_DUPLICATE" {
		t.Fatalf("errorCode: got %q, want OIDC_PROVIDER_DUPLICATE; body=%s", got, raw)
	}
}

func TestOIDC_ActiveOnly_BooleanFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	cid, secret := createM2MClient(t, oidcTenantUUID, "ao-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)

	// Truthy "1" must now filter (previously silently false under string=="true").
	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/oidc/providers?activeOnly=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activeOnly=1: got %d, want 200", resp.StatusCode)
	}
}

func TestOIDC_ActiveOnly_GarbageReturns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	cid, secret := createM2MClient(t, oidcTenantUUID, "ao-bad-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)

	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/oidc/providers?activeOnly=yes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("activeOnly=yes: got %d, want 400 (ParseBool rejects)", resp.StatusCode)
	}
}

// listOidcProviders is auth-only (any authenticated tenant member, D21) — it has
// no admin guard, so a non-admin authed user gets 200, never 403.
func TestOIDC_List_NonAdmin_Returns200Not403(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	cid, secret := createM2MClient(t, oidcTenantUUID, "nonadmin-user", []string{"ROLE_M2M"}) // no ROLE_ADMIN
	token := getToken(t, cid, secret)

	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/oidc/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-admin list: got %d, want 200 (no admin guard on list)", resp.StatusCode)
	}
}

// assertProblemJSON asserts that resp carries an RFC-9457 ProblemDetail envelope
// with the expected HTTP status and errorCode in properties.errorCode.
// It drains and closes the body.
func assertProblemJSON(t *testing.T, resp *http.Response, wantStatus int, wantCode string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, wantStatus, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type: got %q, want application/problem+json", ct)
	}
	var pd struct {
		Status     int            `json:"status"`
		Properties map[string]any `json:"properties"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("unmarshal ProblemDetail: %v; body=%s", err, raw)
	}
	if got := fmt.Sprintf("%v", pd.Properties["errorCode"]); got != wantCode {
		t.Fatalf("errorCode: got %q, want %q; body=%s", got, wantCode, raw)
	}
}

// TestOIDC_Delete_NotFound_ProblemDetail asserts the server emits
// application/problem+json with OIDC_PROVIDER_NOT_FOUND on a delete of an
// unknown provider ID. This validates the real server behaviour that the
// converted spec now documents.
func TestOIDC_Delete_NotFound_ProblemDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	cid, secret := createM2MClient(t, oidcTenantUUID, "del-nf-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)
	req, _ := e2eNewRequest(t, "DELETE", serverURL+"/api/oauth/oidc/providers/00000000-0000-0000-0000-0000000000ff", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertProblemJSON(t, resp, http.StatusNotFound, "OIDC_PROVIDER_NOT_FOUND")
}

// TestOIDC_Register_InvalidTenant_ProblemDetail asserts the server emits
// application/problem+json with OIDC_INVALID_TENANT when the caller's tenant
// is not UUID-shaped. The bootstrap "testclient"/"testsecret" credentials
// belong to the non-UUID "test-tenant", triggering this guard.
func TestOIDC_Register_InvalidTenant_ProblemDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	token := getToken(t, "testclient", "testsecret") // bootstrap tenant = "test-tenant" (non-UUID)
	resp := registerOIDCProvider(t, token, "badtenant")
	assertProblemJSON(t, resp, http.StatusBadRequest, "OIDC_INVALID_TENANT")
}
