package parity

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

type m2mCred struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret"`
}

// createChosen creates a client with a chosen id and returns its credentials.
// The secret is never included in failure messages.
func createChosen(t *testing.T, c *client.Client, id string) m2mCred {
	t.Helper()
	code, body, err := c.CreateClientWithIDRaw(t, id, false, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create %q: %d %v", id, code, err)
	}
	var cred m2mCred
	if err := json.Unmarshal(body, &cred); err != nil || cred.ID != id || cred.Secret == "" {
		t.Fatalf("create %q: unexpected response (id=%q)", id, cred.ID)
	}
	return cred
}

// RunM2MClientSameIDTwoTenants checks that the same client id in two tenants
// names two independent clients: each secret authenticates only at its own
// tenant's token URL, and deleting or resetting one never affects the other.
func RunM2MClientSameIDTwoTenants(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	b := fixture.NewTenant(t)
	ca := client.NewClient(fixture.BaseURL(), a.Token)
	cb := client.NewClient(fixture.BaseURL(), b.Token)
	ctx := context.Background()
	base := fixture.BaseURL()
	const id = "backend"

	credA := createChosen(t, ca, id)
	credB := createChosen(t, cb, id)

	status := func(tenant, secret string) int {
		_, st, _ := client.FetchClientCredentialsToken(ctx, base, tenant, id, secret)
		return st
	}
	if st := status(a.ID, credA.Secret); st != http.StatusOK {
		t.Fatalf("A secret at A: %d", st)
	}
	if st := status(b.ID, credB.Secret); st != http.StatusOK {
		t.Fatalf("B secret at B: %d", st)
	}
	if st := status(b.ID, credA.Secret); st != http.StatusUnauthorized {
		t.Fatalf("A secret at B: %d, want 401", st)
	}
	if st := status(a.ID, credB.Secret); st != http.StatusUnauthorized {
		t.Fatalf("B secret at A: %d, want 401", st)
	}

	// Deleting A's client leaves B's untouched.
	if code, _, _ := ca.DeleteClientRaw(t, id); code != http.StatusOK {
		t.Fatalf("delete in A: %d", code)
	}
	if st := status(a.ID, credA.Secret); st != http.StatusUnauthorized {
		t.Fatalf("deleted A client: %d, want 401", st)
	}
	if st := status(b.ID, credB.Secret); st != http.StatusOK {
		t.Fatalf("B after A delete: %d", st)
	}

	// Re-create in A; resetting B's secret leaves A's new client untouched.
	credA2 := createChosen(t, ca, id)
	code, body, _ := cb.ResetClientSecretRaw(t, id)
	if code != http.StatusOK {
		t.Fatalf("reset in B: %d", code)
	}
	var reset m2mCred
	_ = json.Unmarshal(body, &reset)
	if st := status(a.ID, credA2.Secret); st != http.StatusOK {
		t.Fatalf("A after B reset: %d", st)
	}
	if st := status(b.ID, credB.Secret); st != http.StatusUnauthorized {
		t.Fatalf("B old secret after reset: %d, want 401", st)
	}
	if st := status(b.ID, reset.Secret); st != http.StatusOK {
		t.Fatalf("B new secret: %d", st)
	}
}

// RunM2MClientChosenIDExists checks that a chosen client id that is already
// taken answers 409 M2M_CLIENT_EXISTS and is free again once deleted.
func RunM2MClientChosenIDExists(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	ca := client.NewClient(fixture.BaseURL(), a.Token)
	const id = "order-service"

	createChosen(t, ca, id)

	code, body, _ := ca.CreateClientWithIDRaw(t, id, false, false)
	if code == http.StatusOK {
		// The body carries a live secret: never echo it.
		t.Fatalf("duplicate create unexpectedly succeeded: %d", code)
	}
	if code != http.StatusConflict || !containsErrorCode(body, "M2M_CLIENT_EXISTS") {
		t.Fatalf("duplicate create: %d %s", code, body)
	}

	if code, _, _ := ca.DeleteClientRaw(t, id); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	createChosen(t, ca, id)
}
