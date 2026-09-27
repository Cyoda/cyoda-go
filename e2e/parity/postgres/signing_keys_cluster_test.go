package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// reconcileInterval is the own cluster's CYODA_AUTH_CACHE_RECONCILE_INTERVAL.
// A node whose last successful re-read is older than 10 × this is stale.
const reconcileInterval = time.Second

// TestSigningKeys_OwnCluster revokes the bootstrap key and rotates with
// invalidateCurrent, so it runs on its own cluster: the shared multi-node
// cluster signs its fixture tokens with the bootstrap key.
func TestSigningKeys_OwnCluster(t *testing.T) {
	fix, cleanup := MustSetupMultiNodeWithEnv(t, 2, []string{
		"CYODA_AUTH_CACHE_RECONCILE_INTERVAL=" + reconcileInterval.String(),
		"CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true",
	})
	defer cleanup()
	urls := fix.BaseURLs()
	tenant := fix.NewTenant(t) // bootstrap-signed, ROLE_ADMIN
	bootKID := client.TokenKID(tenant.Token)
	if bootKID == "" {
		t.Fatal("the fixture's tenant token carries no kid")
	}
	a := client.NewClient(urls[0], tenant.Token)

	// Positive control: B accepts the bootstrap-signed tenant token on the
	// probe, so a later 401 is the key under test, not the probe.
	waitStatus(t, urls[1], tenant.Token, http.StatusOK, "control: B accepts the bootstrap-signed tenant token")

	// Token path before touching the bootstrap key: an issued key and an
	// admin M2M client on A, so later calls do not depend on the bootstrap
	// key. Admin tokens come only from adminToken.
	k1 := issueOn(t, a, false)
	adminID, adminSecret := createAdminClient(t, a)
	adminToken := func(t *testing.T) string { return fetchClientToken(t, urls[0], adminID, adminSecret) }
	t1 := adminToken(t)
	if got := client.TokenKID(t1); got != k1 {
		t.Fatalf("A signs with %q, want K1 %q", got, k1)
	}
	waitStatus(t, urls[1], t1, http.StatusOK, "B accepts K1's token")

	t.Run("rotation on A ends the old key and the bootstrap key on B", func(t *testing.T) {
		k2 := issueOn(t, client.NewClient(urls[0], t1), true)
		t2 := adminToken(t)
		if got := client.TokenKID(t2); got != k2 {
			t.Fatalf("A signs with %q, want K2 %q", got, k2)
		}
		waitStatus(t, urls[1], t1, http.StatusUnauthorized, "B refuses K1")
		waitStatus(t, urls[1], tenant.Token, http.StatusUnauthorized, "B refuses the bootstrap key")
		waitStatus(t, urls[1], t2, http.StatusOK, "B accepts K2")
	})

	admin := func(t *testing.T) *client.Client { return client.NewClient(urls[0], adminToken(t)) }

	t.Run("bootstrap reactivate, delete and terminal delete across nodes", func(t *testing.T) {
		if code, _, err := admin(t).ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)); err != nil || code != http.StatusOK {
			t.Fatalf("reactivate bootstrap on A: %d %v", code, err)
		}
		waitStatus(t, urls[1], tenant.Token, http.StatusOK, "B accepts the reactivated bootstrap key")
		if code, _, err := admin(t).DeleteKeyPairRaw(t, bootKID); err != nil || code != http.StatusOK {
			t.Fatalf("delete bootstrap on A: %d %v", code, err)
		}
		waitStatus(t, urls[1], tenant.Token, http.StatusUnauthorized, "B refuses the deleted bootstrap key")
		waitStatus(t, urls[1], adminToken(t), http.StatusOK, "control: B accepts K2 after the bootstrap delete")
		if code, _, err := admin(t).ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)); err != nil || code != http.StatusNotFound {
			t.Fatalf("reactivate after delete: %d %v, want 404", code, err)
		}
		if code, _, err := admin(t).InvalidateKeyPairRaw(t, bootKID); err != nil || code != http.StatusNotFound {
			t.Fatalf("invalidate after delete: %d %v, want 404", code, err)
		}
	})

	t.Run("a node that cannot read its database fails closed", func(t *testing.T) {
		pauser, ok := fix.(interface {
			PauseDatabase(*testing.T)
			UnpauseDatabase(*testing.T)
		})
		if !ok {
			t.Fatal("fixture does not expose PauseDatabase")
		}
		tok := adminToken(t)
		waitStatus(t, urls[1], tok, http.StatusOK, "B accepts the current key")
		if code := statusOf(t, urls[1]+"/api/.well-known/jwks.json", ""); code != http.StatusOK {
			t.Fatalf("control: JWKS on B before the pause: %d, want 200", code)
		}

		pauser.PauseDatabase(t)
		paused := true
		t.Cleanup(func() {
			if paused {
				pauser.UnpauseDatabase(t)
			}
		})
		// Past the staleness bound (10 × interval), with margin.
		time.Sleep(10*reconcileInterval + 3*time.Second)
		if code := statusOf(t, urls[1]+"/api/.well-known/jwks.json", ""); code != http.StatusServiceUnavailable {
			t.Errorf("JWKS on a stale node: %d, want 503", code)
		}
		if code := statusOf(t, urls[1]+"/api/model/", tok); code != http.StatusUnauthorized {
			t.Errorf("stale node on a token of the current key: %d, want 401 (0 = no response)", code)
		}

		pauser.UnpauseDatabase(t)
		paused = false
		waitStatus(t, urls[1], tok, http.StatusOK, "B recovers after the database returns")
		if code := statusOf(t, urls[1]+"/api/.well-known/jwks.json", ""); code != http.StatusOK {
			t.Errorf("JWKS on B after recovery: %d, want 200", code)
		}
	})
}

func issueOn(t *testing.T, c *client.Client, invalidateCurrent bool) string {
	t.Helper()
	body := map[string]any{"algorithm": "RS256", "audience": "client"}
	if invalidateCurrent {
		body["invalidateCurrent"] = true
	}
	code, b, err := c.IssueKeyPairRaw(t, body)
	if err != nil || code != http.StatusOK {
		t.Fatalf("issue: %d %v", code, err)
	}
	var kp struct {
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal(b, &kp); err != nil || kp.KeyID == "" {
		t.Fatalf("issue: no keyId in the response (decode error: %v)", err)
	}
	return kp.KeyID
}

// createAdminClient creates an M2M client with the admin role on the node c
// targets. M2M clients are per node: fetch its tokens from that node.
func createAdminClient(t *testing.T, c *client.Client) (id, secret string) {
	t.Helper()
	code, body, err := c.CreateClientRaw(t, true)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create admin client: %d %v", code, err)
	}
	var cred struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &cred); err != nil || cred.ClientID == "" || cred.ClientSecret == "" {
		t.Fatalf("create admin client: response has no client_id/client_secret (decode error: %v)", err)
	}
	return cred.ClientID, cred.ClientSecret
}

func fetchClientToken(t *testing.T, baseURL, id, secret string) string {
	t.Helper()
	tok, code, err := client.FetchClientCredentialsToken(context.Background(), baseURL, id, secret)
	if err != nil || code != http.StatusOK || tok == "" {
		t.Fatalf("client_credentials token: %d %v", code, err)
	}
	return tok
}

// probeHTTP bounds every probe: while the database is paused, a request the
// node accepts would hang on its database read.
var probeHTTP = &http.Client{Timeout: 10 * time.Second}

// statusOf returns the status of GET u (with the bearer token, if any), or 0
// when no response arrived within probeHTTP's timeout. Never logs the token.
func statusOf(t *testing.T, u, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := probeHTTP.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// waitStatus polls an authenticated read on baseURL until it answers want.
// The bound covers a lost change message: re-reads run every second here.
func waitStatus(t *testing.T, baseURL, token string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := statusOf(t, baseURL+"/api/model/", token)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s (last status %d)", what, got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
