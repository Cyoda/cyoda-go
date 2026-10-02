package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/app"
)

// --- Test cases for the /clients OpenAPI surface ---
//
// All requests go through the chi router via adminRequest (the suite's
// admin token) unless a test names another caller. The M2M-admin-role
// feature flag is enabled in TestMain, so withAdminRole=true is the happy
// path on the shared server; TestE2E_Clients_AdminRoleFlagOff_404 builds a
// stack of its own with the flag off.

// TestE2E_Clients_HelpersDeleteTheirClients pins that createClient and
// createM2MClient delete the client they create when the calling test ends,
// so no test leaves a client behind in the suite tenant or in another one.
func TestE2E_Clients_HelpersDeleteTheirClients(t *testing.T) {
	const otherTenant, otherUser = "client-helper-cleanup", "cleanup-admin"
	var suiteID, otherID string
	t.Run("create", func(t *testing.T) {
		suiteID, _ = createClient(t, false, false)
		otherID, _ = createM2MClient(t, otherTenant, otherUser, false)
	})
	if clientIDsOn(t, serverURL, suiteToken(t))[suiteID] {
		t.Errorf("createClient left client %s behind in the suite tenant", suiteID)
	}
	if clientIDsOn(t, serverURL, adminTokenForTenant(t, otherTenant, otherUser))[otherID] {
		t.Errorf("createM2MClient left client %s behind in tenant %s", otherID, otherTenant)
	}
}

// clientIDsOn returns the ids GET /clients lists for bearer's tenant on the
// server at baseURL — the shared server or a harness stack.
func clientIDsOn(t *testing.T, baseURL, bearer string) map[string]bool {
	t.Helper()
	req, err := e2eNewRequest(t, http.MethodGet, baseURL+"/api/clients", nil)
	if err != nil {
		t.Fatalf("list clients: new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list clients: %d: %s", resp.StatusCode, raw)
	}
	var list []genapi.TechnicalUserDto
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode client list %s: %v", raw, err)
	}
	ids := map[string]bool{}
	for _, c := range list {
		ids[c.ClientId] = true
	}
	return ids
}

// TestE2E_Clients_ListIncludesACreatedClient asserts the list response shape
// and that a client this test creates is listed. It does not assert an empty
// list: other tests share the suite tenant.
func TestE2E_Clients_ListIncludesACreatedClient(t *testing.T) {
	cid, _ := createClient(t, false, false)
	resp := adminRequest(t, "GET", "/clients", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var list []genapi.TechnicalUserDto
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, c := range list {
		if c.ClientId == cid {
			found = true
		}
	}
	if !found {
		t.Errorf("created client %s missing from list: %+v", cid, list)
	}
}

func TestE2E_Clients_CreateListRoundtrip(t *testing.T) {
	// The create response's own fields are asserted here, so the create is
	// made directly; its delete is registered before any assertion can end
	// the test, as createClient does.
	resp := adminRequest(t, "POST", "/clients", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d: %s", resp.StatusCode, raw)
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		t.Fatalf("decode creds: %v", err)
	}
	if creds.ClientId == "" {
		t.Fatal("create response carries no client id")
	}
	deleteClientAtCleanup(t, serverURL, creds.ClientId, func() string { return suiteToken(t) })
	if creds.ClientSecret == "" {
		t.Error("create response carries no client secret")
	}
	if string(creds.GrantType) != "client_credentials" {
		t.Errorf("grant_type: got %q want client_credentials", creds.GrantType)
	}

	// List should include the new client.
	listResp := adminRequest(t, "GET", "/clients", nil)
	defer listResp.Body.Close()
	var list []genapi.TechnicalUserDto
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var found *genapi.TechnicalUserDto
	for i := range list {
		if list[i].ClientId == creds.ClientId {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("created client %s not in list", creds.ClientId)
	}
	if found.CreationDate.IsZero() {
		t.Errorf("CreationDate zero on listed entry")
	}
	if !containsString(found.Roles, "ROLE_M2M") {
		t.Errorf("roles %v missing ROLE_M2M", found.Roles)
	}
}

func TestE2E_Clients_TokenExchangeRoundtrip(t *testing.T) {
	// Create a client, then exchange its credentials for a JWT.
	id, secret := createClient(t, false, false)

	// The harness's getToken helper does the client_credentials grant.
	token := getToken(t, id, secret)
	if token == "" {
		t.Fatal("getToken returned empty string")
	}
	claims := decodeJWTPayload(t, token)
	if claims["sub"] != id {
		t.Errorf("sub: got %v want %v", claims["sub"], id)
	}
	scopes, _ := claims["scopes"].([]any)
	if !containsAnyString(scopes, "ROLE_M2M") {
		t.Errorf("scopes %v missing ROLE_M2M", scopes)
	}
}

func TestE2E_Clients_ResetSecretRotatesAuth(t *testing.T) {
	id, secret := createClient(t, false, false)

	rResp := adminRequest(t, "PUT", "/clients/"+id+"/secret", nil)
	defer rResp.Body.Close()
	if rResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(rResp.Body)
		t.Fatalf("reset status %d: %s", rResp.StatusCode, raw)
	}
	var newCreds genapi.TechnicalUserCredentialsDto
	if err := json.NewDecoder(rResp.Body).Decode(&newCreds); err != nil {
		t.Fatalf("decode new creds: %v", err)
	}
	if newCreds.ClientSecret == secret {
		t.Fatal("reset returned identical secret")
	}

	// Old secret fails — use a direct request instead of getToken (which fatals on error).
	if code := statusForToken(t, id, secret); code != http.StatusUnauthorized {
		t.Errorf("old secret after reset: %d, want 401", code)
	}
	// New secret works.
	if statusForToken(t, id, newCreds.ClientSecret) != http.StatusOK {
		t.Error("new secret should authenticate")
	}
}

func TestE2E_Clients_DeleteInvalidatesToken(t *testing.T) {
	id, secret := createClient(t, false, false)

	delResp := adminRequest(t, "DELETE", "/clients/"+id, nil)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", delResp.StatusCode)
	}

	if code := statusForToken(t, id, secret); code != http.StatusUnauthorized {
		t.Errorf("deleted client's credentials: %d, want 401", code)
	}
}

func TestE2E_Clients_WithAdminRoleFlagOn(t *testing.T) {
	// M2MAdminRoleEnabled=true is set in TestMain.
	id, secret := createClient(t, true, false)

	token := getToken(t, id, secret)
	claims := decodeJWTPayload(t, token)
	scopes, _ := claims["scopes"].([]any)
	if !containsAnyString(scopes, "ROLE_ADMIN") {
		t.Errorf("withAdminRole=true should include ROLE_ADMIN in scopes; got %v", scopes)
	}
	if !containsAnyString(scopes, "ROLE_M2M") {
		t.Errorf("withAdminRole=true should still include ROLE_M2M; got %v", scopes)
	}
}

// TestE2E_Clients_AdminRoleFlagOff_404 asserts that POST
// /clients?withAdminRole=true answers 404 FEATURE_DISABLED on a stack whose
// M2M-admin-role flag is off. TestMain turns the flag on for the shared
// server, so the test builds a stack of its own.
func TestE2E_Clients_AdminRoleFlagOff_404(t *testing.T) {
	h := newCalloutHarness(t, func(cfg *app.Config) { cfg.IAM.M2MAdminRoleEnabled = false })
	resp := h.DoAuth(t, http.MethodPost, "/api/clients?withAdminRole=true", "", "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /clients?withAdminRole=true with the flag off: %d, want 404: %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	if code := problemErrorCode(string(raw)); code != "FEATURE_DISABLED" {
		t.Errorf("errorCode %q, want FEATURE_DISABLED: %s", code, raw)
	}
}

// TestE2E_Clients_NonAdmin_403 asserts that a caller without ROLE_ADMIN gets
// 403 FORBIDDEN from all four /clients operations, and that the client it
// named for delete and reset still authenticates afterwards.
func TestE2E_Clients_NonAdmin_403(t *testing.T) {
	id, secret := createClient(t, false, false)
	nonAdmin, err := signServiceToken(e2eSignKey, e2eIssuer, "", "clients-non-admin", "test-tenant", "clients-non-admin", []string{"ROLE_USER"})
	if err != nil {
		t.Fatalf("sign non-admin token: %v", err)
	}
	for _, op := range []struct{ name, method, path string }{
		{"list", http.MethodGet, "/api/clients"},
		{"create", http.MethodPost, "/api/clients"},
		{"delete", http.MethodDelete, "/api/clients/" + id},
		{"reset", http.MethodPut, "/api/clients/" + id + "/secret"},
	} {
		t.Run(op.name, func(t *testing.T) {
			resp := unauthRequest(t, op.method, op.path, "Bearer "+nonAdmin)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("non-admin %s %s: %d, want 403: %s", op.method, op.path, resp.StatusCode, withheld(resp.StatusCode, raw))
			}
			if code := problemErrorCode(string(raw)); code != "FORBIDDEN" {
				t.Errorf("non-admin %s %s: errorCode %q, want FORBIDDEN: %s", op.method, op.path, code, raw)
			}
		})
	}
	if code := statusForToken(t, id, secret); code != http.StatusOK {
		t.Errorf("client after the non-admin delete and reset: %d, want 200", code)
	}
}

// TestE2E_Clients_IDOutsideGrammar_400 asserts that DELETE and PUT .../secret
// answer 400 BAD_REQUEST for a path id outside the client-id grammar.
func TestE2E_Clients_IDOutsideGrammar_400(t *testing.T) {
	for _, op := range []struct{ name, method, path string }{
		{"delete", http.MethodDelete, "/clients/a-b"},
		{"reset", http.MethodPut, "/clients/a-b/secret"},
	} {
		t.Run(op.name, func(t *testing.T) {
			resp := adminRequest(t, op.method, op.path, nil)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s: %d, want 400: %s", op.method, op.path, resp.StatusCode, withheld(resp.StatusCode, raw))
			}
			if code := problemErrorCode(string(raw)); code != "BAD_REQUEST" {
				t.Errorf("%s %s: errorCode %q, want BAD_REQUEST: %s", op.method, op.path, code, raw)
			}
		})
	}
}

// TestE2E_Clients_CredentialResponsesAreNotCacheable: every response that
// carries a credential — a create's and a reset's plaintext secret, a
// client_credentials token — has Cache-Control: no-store and Pragma: no-cache
// (RFC 6749 §5.1).
func TestE2E_Clients_CredentialResponsesAreNotCacheable(t *testing.T) {
	assertNoStore := func(t *testing.T, what string, resp *http.Response) []byte {
		t.Helper()
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", what, resp.StatusCode, withheld(resp.StatusCode, raw))
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
		}
		if got := resp.Header.Get("Pragma"); got != "no-cache" {
			t.Errorf("%s: Pragma = %q, want no-cache", what, got)
		}
		return raw
	}

	// The header checks report with Errorf, so the delete is registered
	// before anything can end the test with the client left behind.
	cred := decodeCredential(t, "create client", assertNoStore(t, "create",
		adminRequest(t, http.MethodPost, "/clients", nil)))
	deleteClientAtCleanup(t, serverURL, cred.id, func() string { return suiteToken(t) })

	reset := decodeCredential(t, "reset secret", assertNoStore(t, "reset",
		adminRequest(t, http.MethodPut, "/clients/"+cred.id+"/secret", nil)))

	assertNoStore(t, "client_credentials token",
		postToken(t, url.Values{"grant_type": {"client_credentials"}}, reset.id, reset.secret))
}

// TestE2E_Clients_OnBehalfOf_Create: POST /clients?onBehalfOf=true creates an
// on-behalf-of client whose credentials carry onBehalfOf=true and the
// token-exchange grant_type; the list entry and the reset response carry the
// same shape.
func TestE2E_Clients_OnBehalfOf_Create(t *testing.T) {
	resp := adminRequest(t, "POST", "/clients?onBehalfOf=true", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status %d: %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("decode creds: %v", err)
	}
	deleteClientAtCleanup(t, serverURL, creds.ClientId, func() string { return suiteToken(t) })
	if !creds.OnBehalfOf {
		t.Error("create response: onBehalfOf false, want true")
	}
	if string(creds.GrantType) != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Errorf("grant_type: got %q, want the token-exchange URN", creds.GrantType)
	}
	if len(creds.Roles) != 1 || creds.Roles[0] != "ROLE_M2M" {
		t.Errorf("roles: got %v want [ROLE_M2M]", creds.Roles)
	}

	listResp := adminRequest(t, "GET", "/clients", nil)
	defer listResp.Body.Close()
	var list []genapi.TechnicalUserDto
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var found *genapi.TechnicalUserDto
	for i := range list {
		if list[i].ClientId == creds.ClientId {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("created client %s not in list", creds.ClientId)
	}
	if !found.OnBehalfOf {
		t.Error("list entry: onBehalfOf false, want true")
	}

	resetResp := adminRequest(t, "PUT", "/clients/"+creds.ClientId+"/secret", nil)
	defer resetResp.Body.Close()
	resetRaw, _ := io.ReadAll(resetResp.Body)
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("reset status %d: %s", resetResp.StatusCode, withheld(resetResp.StatusCode, resetRaw))
	}
	var resetCreds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(resetRaw, &resetCreds); err != nil {
		t.Fatalf("decode reset creds: %v", err)
	}
	if !resetCreds.OnBehalfOf {
		t.Error("reset response: onBehalfOf false, want true")
	}
	if string(resetCreds.GrantType) != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Errorf("reset grant_type: got %q, want the token-exchange URN", resetCreds.GrantType)
	}
}

// TestE2E_Clients_OnBehalfOfWithAdminRole_400: withAdminRole=true combined
// with onBehalfOf=true is refused, and no client is created.
func TestE2E_Clients_OnBehalfOfWithAdminRole_400(t *testing.T) {
	before := clientIDsOn(t, serverURL, suiteToken(t))
	resp := adminRequest(t, "POST", "/clients?withAdminRole=true&onBehalfOf=true", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	if code := problemErrorCode(string(raw)); code != "BAD_REQUEST" {
		t.Errorf("errorCode %q, want BAD_REQUEST: %s", code, raw)
	}
	after := clientIDsOn(t, serverURL, suiteToken(t))
	if len(after) != len(before) {
		t.Errorf("a client was created despite the 400: before %v after %v", before, after)
	}
}

// TestE2E_Clients_OnBehalfOfInPlatform_400: onBehalfOf=true in the PLATFORM
// tenant is refused, and no client is created there.
func TestE2E_Clients_OnBehalfOfInPlatform_400(t *testing.T) {
	before := clientIDsOn(t, serverURL, platformToken(t))
	resp := operatorRequest(t, "POST", "/clients?onBehalfOf=true", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	if code := problemErrorCode(string(raw)); code != "BAD_REQUEST" {
		t.Errorf("errorCode %q, want BAD_REQUEST: %s", code, raw)
	}
	after := clientIDsOn(t, serverURL, platformToken(t))
	if len(after) != len(before) {
		t.Errorf("a client was created in PLATFORM despite the 400: before %v after %v", before, after)
	}
}

// TestE2E_Clients_NoToken_401 asserts that every /clients operation answers
// 401 UNAUTHORIZED with no Authorization header at all — distinct from the
// 403 a non-admin token gets (TestE2E_Clients_NonAdmin_403).
func TestE2E_Clients_NoToken_401(t *testing.T) {
	id, secret := createClient(t, false, false)
	for _, op := range []struct{ name, method, path string }{
		{"list", http.MethodGet, "/api/clients"},
		{"create", http.MethodPost, "/api/clients"},
		{"delete", http.MethodDelete, "/api/clients/" + id},
		{"reset", http.MethodPut, "/api/clients/" + id + "/secret"},
	} {
		t.Run(op.name, func(t *testing.T) {
			resp := unauthRequest(t, op.method, op.path, "")
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("no token %s %s: %d, want 401: %s", op.method, op.path, resp.StatusCode, withheld(resp.StatusCode, raw))
			}
			if code := problemErrorCode(string(raw)); code != "UNAUTHORIZED" {
				t.Errorf("no token %s %s: errorCode %q, want UNAUTHORIZED: %s", op.method, op.path, code, raw)
			}
		})
	}
	if code := statusForToken(t, id, secret); code != http.StatusOK {
		t.Errorf("client after the no-token attempts: %d, want 200", code)
	}
}

// --- local helpers (not exported into the wider e2e harness) ---

// statusForToken issues a /oauth/token request with the given creds and
// returns the HTTP status code (does not fatal on non-200, unlike getToken).
func statusForToken(t *testing.T, clientID, clientSecret string) int {
	t.Helper()
	return tokenStatusOn(t, serverURL, clientID, clientSecret)
}

// tokenStatusOn is statusForToken against the server at baseURL — the shared
// server or a harness stack.
func tokenStatusOn(t *testing.T, baseURL, clientID, clientSecret string) int {
	t.Helper()
	resp := postTokenTo(t, baseURL, url.Values{"grant_type": {"client_credentials"}}, clientID, clientSecret)
	defer resp.Body.Close()
	return resp.StatusCode
}

// decodeJWTPayload extracts and decodes the JWT payload (middle segment) without verification.
func decodeJWTPayload(t *testing.T, tokenStr string) map[string]any {
	t.Helper()
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed JWT: %s", tokenStr)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

// TestE2E_Clients_CrossTenantIsolation_404 verifies that tenant B's admin
// cannot see, delete or reset-secret a client owned by tenant A, and cannot
// tell it from a client that does not exist: DELETE and PUT .../secret answer
// 404 M2M_CLIENT_NOT_FOUND with the body an absent id gets (instance, the
// request path, aside), and B's GET /clients does not list it. The unit tests
// mock the tenant context directly; this E2E case proves the JWT-claim
// extraction in the auth middleware correctly gates the chi adapter.
func TestE2E_Clients_CrossTenantIsolation_404(t *testing.T) {
	// Tenant A is the suite tenant, "test-tenant".
	clientA, secretA := createClient(t, false, false)
	clientB, secretB := createM2MClient(t, "tenant-b", "user-b", true)
	absent := "ABSENT" + strings.ToUpper(randSuffix(t))

	for _, op := range []struct{ method, suffix string }{
		{http.MethodDelete, ""},
		{http.MethodPut, "/secret"},
	} {
		var bodies [2]map[string]any
		for i, id := range []string{clientA, absent} {
			resp := adminRequestAs(t, clientB, secretB, op.method, "/clients/"+id+op.suffix, nil)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("tenant B %s /clients/{id}%s (id %d of [A's, absent]): %d, want 404: %s", op.method, op.suffix, i, resp.StatusCode, withheld(resp.StatusCode, raw))
			}
			if code := problemErrorCode(string(raw)); code != "M2M_CLIENT_NOT_FOUND" {
				t.Fatalf("tenant B %s /clients/{id}%s (id %d of [A's, absent]): errorCode %q, want M2M_CLIENT_NOT_FOUND: %s", op.method, op.suffix, i, code, raw)
			}
			if err := json.Unmarshal(raw, &bodies[i]); err != nil {
				t.Fatalf("decode problem %s: %v", raw, err)
			}
			delete(bodies[i], "instance")
		}
		if !reflect.DeepEqual(bodies[0], bodies[1]) {
			t.Errorf("tenant B %s /clients/{id}%s: A's client answers %v, an absent id %v — an existence oracle", op.method, op.suffix, bodies[0], bodies[1])
		}
	}

	if clientIDsOn(t, serverURL, adminTokenForTenant(t, "tenant-b", "user-b"))[clientA] {
		t.Errorf("tenant B's GET /clients lists tenant A's client %s", clientA)
	}
	// B's attempts left A's client as it was.
	if code := statusForToken(t, clientA, secretA); code != http.StatusOK {
		t.Errorf("tenant A's client after tenant B's attempts: %d, want 200", code)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsAnyString(haystack []any, needle string) bool {
	for _, s := range haystack {
		if str, ok := s.(string); ok && str == needle {
			return true
		}
	}
	return false
}
