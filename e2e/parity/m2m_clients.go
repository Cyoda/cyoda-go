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

	code, body, err := ca.CreateClientRaw(t, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create: %d %v", code, err)
	}
	var cred struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	_ = json.Unmarshal(body, &cred)
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, cred.Secret); st != http.StatusOK {
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
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, cred.Secret); st != http.StatusUnauthorized {
		t.Fatalf("old secret: %d", st)
	}
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, reset.Secret); st != http.StatusOK {
		t.Fatalf("new secret: %d", st)
	}
	if code, _, _ := ca.DeleteClientRaw(t, cred.ID); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, reset.Secret); st != http.StatusUnauthorized {
		t.Fatalf("deleted: %d", st)
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
		code, body, _ := c.CreateClientRaw(t, false)
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
	code, body, _ := c.CreateClientRaw(t, false)
	if code == http.StatusOK {
		// The cap failed to bind — the response carries a live
		// client_secret, so never echo the body into the failure message.
		t.Fatalf("fourth: %d, want %d (cap not enforced)", code, http.StatusBadRequest)
	}
	if code != http.StatusBadRequest || !containsErrorCode(body, "M2M_CLIENT_CAP_REACHED") {
		t.Fatalf("fourth: %d %s", code, body)
	}
	if code, _, _ := c.DeleteClientRaw(t, first); code != http.StatusOK {
		t.Fatal("delete")
	}
	if code, _, _ := c.CreateClientRaw(t, false); code != http.StatusOK {
		t.Fatal("create after delete")
	}
}
