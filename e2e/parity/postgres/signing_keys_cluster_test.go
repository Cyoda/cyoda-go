package postgres

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/multinode"
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

	// k2 is the key the rotation issues; k2Token fetches an admin token from
	// A and checks A signed it with K2.
	var k2 string
	k2Token := func(t *testing.T) string {
		t.Helper()
		tok := adminToken(t)
		if got := client.TokenKID(tok); got != k2 {
			t.Fatalf("A signs with %q, want K2 %q", got, k2)
		}
		return tok
	}

	t.Run("rotation on A ends the old key and the bootstrap key on B", func(t *testing.T) {
		k2 = issueOn(t, client.NewClient(urls[0], t1), true)
		t2 := k2Token(t)
		waitStatus(t, urls[1], t1, http.StatusUnauthorized, "B refuses K1")
		waitStatus(t, urls[1], tenant.Token, http.StatusUnauthorized, "B refuses the bootstrap key")
		waitStatus(t, urls[1], t2, http.StatusOK, "B accepts K2")
	})

	// admin does not require K2: a reactivated bootstrap key gets validFrom =
	// now, so it signs on A (latest validFrom wins) until it is deleted.
	admin := func(t *testing.T, node int) *client.Client { return client.NewClient(urls[node], adminToken(t)) }

	t.Run("bootstrap reactivate, delete and terminal delete across nodes", func(t *testing.T) {
		if code, _, err := admin(t, 0).ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)); err != nil || code != http.StatusOK {
			t.Fatalf("reactivate bootstrap on A: %d %v", code, err)
		}
		waitStatus(t, urls[1], tenant.Token, http.StatusOK, "B accepts the reactivated bootstrap key")
		if code, _, err := admin(t, 0).DeleteKeyPairRaw(t, bootKID); err != nil || code != http.StatusOK {
			t.Fatalf("delete bootstrap on A: %d %v", code, err)
		}
		waitStatus(t, urls[1], tenant.Token, http.StatusUnauthorized, "B refuses the deleted bootstrap key")
		waitStatus(t, urls[1], k2Token(t), http.StatusOK, "control: B accepts K2 after the bootstrap delete")
		// The deleted bootstrap state is terminal on both nodes.
		for node, name := range []string{"A", "B"} {
			if code, _, err := admin(t, node).ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)); err != nil || code != http.StatusNotFound {
				t.Errorf("reactivate after delete on %s: %d %v, want 404", name, code, err)
			}
			if code, _, err := admin(t, node).InvalidateKeyPairRaw(t, bootKID); err != nil || code != http.StatusNotFound {
				t.Errorf("invalidate after delete on %s: %d %v, want 404", name, code, err)
			}
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
		tok := k2Token(t)
		waitStatus(t, urls[1], tok, http.StatusOK, "B accepts the current key")
		if code, _, _ := getJWKS(t, urls[1]); code != http.StatusOK {
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
		code, header, body := getJWKS(t, urls[1])
		multinode.AssertProblem(t, code, body, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", true)
		if header.Get("Retry-After") == "" {
			t.Errorf("JWKS 503 on a stale node carries no Retry-After header")
		}
		if code := modelListStatus(t, urls[1], tok); code != http.StatusUnauthorized {
			t.Errorf("stale node on a token of the current key: %d, want 401 (0 = no response)", code)
		}

		pauser.UnpauseDatabase(t)
		paused = false
		waitStatus(t, urls[1], tok, http.StatusOK, "B recovers after the database returns")
		if code, _, _ := getJWKS(t, urls[1]); code != http.StatusOK {
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

// getJWKS fetches baseURL's public JWKS: status (0 when no response arrived
// within probeHTTP's timeout), headers and body.
func getJWKS(t *testing.T, baseURL string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/.well-known/jwks.json", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := probeHTTP.Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// modelListStatus returns the status of an authenticated read on baseURL
// (200 when the node accepts token, 401 when it refuses it), or 0 when no
// response arrived within probeHTTP's timeout. Never logs the token.
func modelListStatus(t *testing.T, baseURL, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/model/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
		got := modelListStatus(t, baseURL, token)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s (last status %d)", what, got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
