package parity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunM2MClientLifecycle drives an M2M client through create, token, list,
// reset and delete, and checks tenant isolation at every step: another
// tenant's clientId is 404 M2M_CLIENT_NOT_FOUND on delete/reset, and never
// appears in that other tenant's list.
func RunM2MClientLifecycle(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	b := fixture.NewTenant(t)
	ca := client.NewClient(fixture.BaseURL(), a.Token)
	cb := client.NewClient(fixture.BaseURL(), b.Token)
	ctx := context.Background()

	code, body, err := ca.CreateClientRaw(t, false, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create: %d %v", code, err)
	}
	var cred struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	_ = json.Unmarshal(body, &cred)
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), a.ID, cred.ID, cred.Secret); st != http.StatusOK {
		t.Fatalf("token: %d", st)
	}
	if code, body, _ := ca.ListClientsRaw(t); code != http.StatusOK || !strings.Contains(string(body), cred.ID) {
		t.Fatalf("list: %d", code)
	}
	if code, body, _ := cb.ListClientsRaw(t); code != http.StatusOK || strings.Contains(string(body), cred.ID) {
		t.Fatalf("other tenant's list: %d", code)
	}
	for name, call := range map[string]func() (int, []byte, error){
		"delete": func() (int, []byte, error) { return cb.DeleteClientRaw(t, cred.ID) },
		"reset":  func() (int, []byte, error) { return cb.ResetClientSecretRaw(t, cred.ID) },
	} {
		code, body, _ := call()
		if code == http.StatusOK {
			// A 200 here means tenant isolation broke and the response body
			// carries a live client_secret (reset) or a success ack
			// (delete) — never echo it into the failure message.
			t.Fatalf("other tenant %s unexpectedly succeeded (tenant isolation broken): %d", name, code)
		}
		if code != http.StatusNotFound || !containsErrorCode(body, "M2M_CLIENT_NOT_FOUND") {
			t.Fatalf("other tenant %s: %d %s", name, code, body)
		}
	}
	code, body, _ = ca.ResetClientSecretRaw(t, cred.ID)
	if code != http.StatusOK {
		t.Fatalf("reset: %d", code)
	}
	var reset struct {
		Secret string `json:"client_secret"`
	}
	_ = json.Unmarshal(body, &reset)
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), a.ID, cred.ID, cred.Secret); st != http.StatusUnauthorized {
		t.Fatalf("old secret: %d", st)
	}
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), a.ID, cred.ID, reset.Secret); st != http.StatusOK {
		t.Fatalf("new secret: %d", st)
	}
	if code, _, _ := ca.DeleteClientRaw(t, cred.ID); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), a.ID, cred.ID, reset.Secret); st != http.StatusUnauthorized {
		t.Fatalf("deleted: %d", st)
	}
}

// RunM2MClientOnBehalfOf checks that an on-behalf-of (OBO) client — created
// via POST /clients?onBehalfOf=true — reports onBehalfOf=true and the
// token-exchange grant_type on create, in the tenant's list, and again after
// a secret reset, consistently across backends.
func RunM2MClientOnBehalfOf(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	ca := client.NewClient(fixture.BaseURL(), a.Token)

	code, body, err := ca.CreateClientRaw(t, false, true)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create: %d %v", code, err)
	}
	var cred struct {
		ID         string `json:"client_id"`
		Secret     string `json:"client_secret"`
		OnBehalfOf bool   `json:"onBehalfOf"`
		GrantType  string `json:"grant_type"`
	}
	if err := json.Unmarshal(body, &cred); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !cred.OnBehalfOf {
		t.Fatalf("create: onBehalfOf=%v, want true", cred.OnBehalfOf)
	}
	if cred.GrantType != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Fatalf("create: grant_type=%q, want the token-exchange URN", cred.GrantType)
	}

	code, body, _ = ca.ListClientsRaw(t)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	var list []struct {
		ClientID   string `json:"clientId"`
		OnBehalfOf bool   `json:"onBehalfOf"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, c := range list {
		if c.ClientID == cred.ID {
			found = true
			if !c.OnBehalfOf {
				t.Fatalf("list entry %s: onBehalfOf=%v, want true", c.ClientID, c.OnBehalfOf)
			}
		}
	}
	if !found {
		t.Fatalf("created client %s not in list", cred.ID)
	}

	code, body, _ = ca.ResetClientSecretRaw(t, cred.ID)
	if code != http.StatusOK {
		t.Fatalf("reset: %d", code)
	}
	var reset struct {
		OnBehalfOf bool   `json:"onBehalfOf"`
		GrantType  string `json:"grant_type"`
	}
	if err := json.Unmarshal(body, &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if !reset.OnBehalfOf {
		t.Fatalf("reset: onBehalfOf=%v, want true", reset.OnBehalfOf)
	}
	if reset.GrantType != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Fatalf("reset: grant_type=%q, want the token-exchange URN", reset.GrantType)
	}
}

// RunM2MClientCap checks the per-tenant cap: the fixture sets
// CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT=3, so a tenant's third client succeeds,
// its fourth is refused with 400 M2M_CLIENT_CAP_REACHED, and deleting one
// client frees a slot for a fifth create.
func RunM2MClientCap(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), a.Token)
	var first string
	for i := 0; i < 3; i++ {
		code, body, _ := c.CreateClientRaw(t, false, false)
		if code != http.StatusOK {
			t.Fatalf("create %d: %d %s", i, code, body)
		}
		if i == 0 {
			var cred struct {
				ID string `json:"client_id"`
			}
			_ = json.Unmarshal(body, &cred)
			first = cred.ID
		}
	}
	code, body, _ := c.CreateClientRaw(t, false, false)
	if code == http.StatusOK {
		// The cap failed to bind — the response carries a live
		// client_secret, so never echo the body into the failure message.
		t.Fatalf("fourth: %d, want %d (cap not enforced)", code, http.StatusBadRequest)
	}
	if code != http.StatusBadRequest || !containsErrorCode(body, "M2M_CLIENT_CAP_REACHED") {
		t.Fatalf("fourth: %d %s", code, body)
	}
	// A chosen id that is already taken is refused as taken, not as over the
	// cap, even though the tenant is at the cap.
	code, body, _ = c.CreateClientWithIDRaw(t, first, false, false)
	if code == http.StatusOK {
		t.Fatalf("create of a taken id at the cap: %d, want %d", code, http.StatusConflict)
	}
	if code != http.StatusConflict || !containsErrorCode(body, "M2M_CLIENT_EXISTS") {
		t.Fatalf("create of a taken id at the cap: %d %s", code, body)
	}
	if code, _, _ := c.DeleteClientRaw(t, first); code != http.StatusOK {
		t.Fatal("delete")
	}
	if code, _, _ := c.CreateClientRaw(t, false, false); code != http.StatusOK {
		t.Fatal("create after delete")
	}
}
