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
