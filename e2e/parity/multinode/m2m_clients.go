package multinode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(NamedTest{Name: "M2MClientVisibleAcrossNodes", Fn: RunM2MClientVisibleAcrossNodes})
}

// RunM2MClientVisibleAcrossNodes: an M2M client is a KV-store record shared
// by the whole cluster — there is no per-node copy (internal/auth/kv_m2m_store.go),
// so a client created on node A is immediately usable for a token on node B,
// with no poll. A secret reset and a delete on A are each visible on B the
// same way, at once.
func RunM2MClientVisibleAcrossNodes(t *testing.T, fixture MultiNodeFixture) {
	urls := fixture.BaseURLs()
	tenant := fixture.NewTenant(t)
	a := client.NewClient(urls[0], tenant.Token)
	ctx := context.Background()

	code, body, err := a.CreateClientRaw(t, false, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create on A: %d %v", code, err)
	}
	var cred struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &cred); err != nil || cred.ID == "" || cred.Secret == "" {
		t.Fatalf("create on A: response has no client_id/client_secret (decode error: %v)", err)
	}

	if _, st, err := client.FetchClientCredentialsToken(ctx, urls[1], cred.ID, cred.Secret); err != nil || st != http.StatusOK {
		t.Fatalf("token on B for a client created on A: %d %v, want 200", st, err)
	}

	resetCode, resetBody, err := a.ResetClientSecretRaw(t, cred.ID)
	if err != nil || resetCode != http.StatusOK {
		t.Fatalf("reset on A: %d %v", resetCode, err)
	}
	var reset struct {
		Secret string `json:"client_secret"`
	}
	if err := json.Unmarshal(resetBody, &reset); err != nil || reset.Secret == "" {
		t.Fatalf("reset on A: response has no client_secret (decode error: %v)", err)
	}
	if _, st, err := client.FetchClientCredentialsToken(ctx, urls[1], cred.ID, cred.Secret); err != nil || st != http.StatusUnauthorized {
		t.Fatalf("old secret on B after reset on A: %d %v, want 401", st, err)
	}
	if _, st, err := client.FetchClientCredentialsToken(ctx, urls[1], cred.ID, reset.Secret); err != nil || st != http.StatusOK {
		t.Fatalf("new secret on B after reset on A: %d %v, want 200", st, err)
	}

	if delCode, _, err := a.DeleteClientRaw(t, cred.ID); err != nil || delCode != http.StatusOK {
		t.Fatalf("delete on A: %d %v", delCode, err)
	}
	if _, st, err := client.FetchClientCredentialsToken(ctx, urls[1], cred.ID, reset.Secret); err != nil || st != http.StatusUnauthorized {
		t.Fatalf("deleted client's token on B: %d %v, want 401", st, err)
	}
}
