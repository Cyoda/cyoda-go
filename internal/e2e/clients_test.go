package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// --- Test cases for the /clients OpenAPI surface ---
//
// All requests go through the chi router via adminRequest (bootstrap
// admin token). The M2M-admin-role feature flag is enabled in TestMain
// so withAdminRole=true is the happy path here; the flag-off case is
// covered by the unit suite per spec D9.

func TestE2E_Clients_ListEmpty(t *testing.T) {
	// The bootstrap M2M client lives in the store too; List returns it.
	// We only assert the response shape, not emptiness.
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
	// The bootstrap client `testclient` should appear with roles
	// including ROLE_ADMIN (set in TestMain via CYODA_BOOTSTRAP_ROLES default).
	found := false
	for _, c := range list {
		if c.ClientId == "testclient" {
			found = true
		}
	}
	if !found {
		t.Errorf("bootstrap client testclient missing from list: %+v", list)
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
	if statusForToken(t, creds.ClientId, creds.ClientSecret) == http.StatusOK {
		t.Error("old secret still authenticates after reset")
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

	if statusForToken(t, creds.ClientId, creds.ClientSecret) == http.StatusOK {
		t.Error("deleted client's credentials still authenticate")
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
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	body := strings.NewReader(data.Encode())
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/token", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	creds := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	req.Header.Set("Authorization", "Basic "+creds)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
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

// fmt kept for diagnostic use in failure messages above.
var _ = fmt.Sprintf
