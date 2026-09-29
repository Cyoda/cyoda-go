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
)

// --- Test cases for the /clients OpenAPI surface ---
//
// All requests go through the chi router via adminRequest (the suite's
// admin token). The M2M-admin-role feature flag is enabled in TestMain
// so withAdminRole=true is the happy path here; the flag-off case is
// covered by the unit suite per spec D9.

// TestE2E_Clients_HelpersDeleteTheirClients pins that createClient and
// createM2MClient delete the client they create when the calling test ends,
// so no test leaves a client behind in the suite tenant or in another one.
func TestE2E_Clients_HelpersDeleteTheirClients(t *testing.T) {
	const otherTenant, otherUser = "client-helper-cleanup", "cleanup-admin"
	var suiteID, otherID string
	t.Run("create", func(t *testing.T) {
		suiteID, _ = createClient(t, false)
		otherID, _ = createM2MClient(t, otherTenant, otherUser, []string{"ROLE_M2M"})
	})
	if listsClient(t, suiteToken(t), suiteID) {
		t.Errorf("createClient left client %s behind in the suite tenant", suiteID)
	}
	if listsClient(t, adminTokenForTenant(t, otherTenant, otherUser), otherID) {
		t.Errorf("createM2MClient left client %s behind in tenant %s", otherID, otherTenant)
	}
}

// listsClient reports whether GET /clients, called with bearer, lists id.
func listsClient(t *testing.T, bearer, id string) bool {
	t.Helper()
	resp := unauthRequest(t, http.MethodGet, "/api/clients", "Bearer "+bearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("list clients: %d: %s", resp.StatusCode, raw)
	}
	var list []genapi.TechnicalUserDto
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode client list: %v", err)
	}
	for _, c := range list {
		if c.ClientId == id {
			return true
		}
	}
	return false
}

func TestE2E_Clients_ListEmpty(t *testing.T) {
	// We only assert the response shape and that a client this test creates
	// is listed, not emptiness (other tests in the run may have left clients
	// behind).
	cid, _ := createClient(t, false)
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
	if creds.ClientId == "" || creds.ClientSecret == "" {
		t.Fatalf("creds blank: %+v", creds)
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

	// Cleanup.
	delResp := adminRequest(t, "DELETE", "/clients/"+creds.ClientId, nil)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Errorf("delete: %d", delResp.StatusCode)
	}
}

func TestE2E_Clients_TokenExchangeRoundtrip(t *testing.T) {
	// Create a client, then exchange its credentials for a JWT.
	resp := adminRequest(t, "POST", "/clients", nil)
	defer resp.Body.Close()
	var creds genapi.TechnicalUserCredentialsDto
	_ = json.NewDecoder(resp.Body).Decode(&creds)

	// The harness's getToken helper does the client_credentials grant.
	token := getToken(t, creds.ClientId, creds.ClientSecret)
	if token == "" {
		t.Fatal("getToken returned empty string")
	}
	claims := decodeJWTPayload(t, token)
	if claims["sub"] != creds.ClientId {
		t.Errorf("sub: got %v want %v", claims["sub"], creds.ClientId)
	}
	scopes, _ := claims["scopes"].([]any)
	if !containsAnyString(scopes, "ROLE_M2M") {
		t.Errorf("scopes %v missing ROLE_M2M", scopes)
	}

	// Cleanup.
	adminRequest(t, "DELETE", "/clients/"+creds.ClientId, nil).Body.Close()
}

func TestE2E_Clients_ResetSecretRotatesAuth(t *testing.T) {
	resp := adminRequest(t, "POST", "/clients", nil)
	defer resp.Body.Close()
	var creds genapi.TechnicalUserCredentialsDto
	_ = json.NewDecoder(resp.Body).Decode(&creds)

	rResp := adminRequest(t, "PUT", "/clients/"+creds.ClientId+"/secret", nil)
	defer rResp.Body.Close()
	if rResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(rResp.Body)
		t.Fatalf("reset status %d: %s", rResp.StatusCode, raw)
	}
	var newCreds genapi.TechnicalUserCredentialsDto
	if err := json.NewDecoder(rResp.Body).Decode(&newCreds); err != nil {
		t.Fatalf("decode new creds: %v", err)
	}
	if newCreds.ClientSecret == creds.ClientSecret {
		t.Fatal("reset returned identical secret")
	}

	// Old secret fails — use a direct request instead of getToken (which fatals on error).
	if code := statusForToken(t, creds.ClientId, creds.ClientSecret); code != http.StatusUnauthorized {
		t.Errorf("old secret after reset: %d, want 401", code)
	}
	// New secret works.
	if statusForToken(t, creds.ClientId, newCreds.ClientSecret) != http.StatusOK {
		t.Error("new secret should authenticate")
	}

	// Cleanup.
	adminRequest(t, "DELETE", "/clients/"+creds.ClientId, nil).Body.Close()
}

func TestE2E_Clients_DeleteInvalidatesToken(t *testing.T) {
	resp := adminRequest(t, "POST", "/clients", nil)
	defer resp.Body.Close()
	var creds genapi.TechnicalUserCredentialsDto
	_ = json.NewDecoder(resp.Body).Decode(&creds)

	delResp := adminRequest(t, "DELETE", "/clients/"+creds.ClientId, nil)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", delResp.StatusCode)
	}

	if code := statusForToken(t, creds.ClientId, creds.ClientSecret); code != http.StatusUnauthorized {
		t.Errorf("deleted client's credentials: %d, want 401", code)
	}
}

func TestE2E_Clients_WithAdminRoleFlagOn(t *testing.T) {
	// M2MAdminRoleEnabled=true is set in TestMain.
	resp := adminRequest(t, "POST", "/clients?withAdminRole=true", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var creds genapi.TechnicalUserCredentialsDto
	_ = json.NewDecoder(resp.Body).Decode(&creds)

	token := getToken(t, creds.ClientId, creds.ClientSecret)
	claims := decodeJWTPayload(t, token)
	scopes, _ := claims["scopes"].([]any)
	if !containsAnyString(scopes, "ROLE_ADMIN") {
		t.Errorf("withAdminRole=true should include ROLE_ADMIN in scopes; got %v", scopes)
	}
	if !containsAnyString(scopes, "ROLE_M2M") {
		t.Errorf("withAdminRole=true should still include ROLE_M2M; got %v", scopes)
	}

	// Cleanup.
	adminRequest(t, "DELETE", "/clients/"+creds.ClientId, nil).Body.Close()
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
	clientA, secretA := createClient(t, false)
	clientB, secretB := createM2MClient(t, "tenant-b", "user-b", []string{"ROLE_ADMIN", "ROLE_M2M"})
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

	if listsClient(t, adminTokenForTenant(t, "tenant-b", "user-b"), clientA) {
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

