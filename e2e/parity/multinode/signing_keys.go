package multinode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(NamedTest{Name: "SigningKeyPairFollowsTheCluster", Fn: RunSigningKeyPairFollowsTheCluster})
}

// keyPairChangeBound is how long another node may take to apply a key-pair
// change: the gossip message normally arrives well under a second; if it is
// lost, the next periodic re-read (at most 1.1 × the default 60 s interval)
// applies it.
const keyPairChangeBound = 70 * time.Second

// eventually polls cond every 200 ms until it holds or keyPairChangeBound
// passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(keyPairChangeBound)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

var probeHTTP = &http.Client{Timeout: 30 * time.Second}

// modelListStatus is the status of an authenticated read on baseURL with
// token: 200 when the node accepts the token, 401 when it refuses it.
// Never logs the token.
func modelListStatus(t *testing.T, baseURL, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/model/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := probeHTTP.Do(req)
	if err != nil {
		t.Fatalf("GET /api/model/: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// newM2MClient creates an M2M client through the node c targets.
func newM2MClient(t *testing.T, c *client.Client) (id, secret string) {
	t.Helper()
	code, body, err := c.CreateClientRaw(t, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create M2M client: %d %v", code, err)
	}
	var cred struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &cred); err != nil || cred.ClientID == "" || cred.ClientSecret == "" {
		t.Fatalf("create M2M client: response has no client_id/client_secret (decode error: %v)", err)
	}
	return cred.ClientID, cred.ClientSecret
}

// IssueClientKeyPair issues an RS256 key pair on the node c targets, as a
// rotation when invalidateCurrent is set, and returns its key id.
func IssueClientKeyPair(t *testing.T, c *client.Client, invalidateCurrent bool) string {
	t.Helper()
	body := map[string]any{"algorithm": "RS256"}
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

// clientToken runs the client_credentials grant on baseURL.
func clientToken(t *testing.T, baseURL, id, secret string) string {
	t.Helper()
	tok, code, err := client.FetchClientCredentialsToken(context.Background(), baseURL, id, secret)
	if err != nil || code != http.StatusOK || tok == "" {
		t.Fatalf("client_credentials token: %d %v", code, err)
	}
	return tok
}

// RunSigningKeyPairFollowsTheCluster: a key pair issued on node A signs on A
// and, once B has the change, verifies on B, is in B's JWKS and signs on B;
// invalidate, reactivate and delete on A take effect on B.
//
// The shared cluster signs its fixture tokens with the bootstrap key, so the
// scenario issues without invalidateCurrent: the bootstrap key stays valid
// for later scenarios. The key pair is deleted at the end (and on cleanup if
// the scenario fails part-way).
func RunSigningKeyPairFollowsTheCluster(t *testing.T, fixture MultiNodeFixture) {
	urls := fixture.BaseURLs()
	tenant := fixture.NewTenant(t)
	op := client.NewClient(urls[0], fixture.PlatformOperator(t).Token)
	a := client.NewClient(urls[0], tenant.Token)
	b := client.NewClient(urls[1], tenant.Token)

	// Positive controls: the probe accepts the tenant token and an M2M
	// token on B, so a later 401 is the key under test, not the probe.
	if code := modelListStatus(t, urls[1], tenant.Token); code != http.StatusOK {
		t.Fatalf("control: B with the tenant token: %d, want 200", code)
	}
	id, secret := newM2MClient(t, a)
	bootTok := clientToken(t, urls[0], id, secret)
	if code := modelListStatus(t, urls[1], bootTok); code != http.StatusOK {
		t.Fatalf("control: B with an M2M token signed before the issue: %d, want 200", code)
	}

	kid := IssueClientKeyPair(t, op, false)
	op.DeleteKeyPairOnCleanup(t, kid)

	tok := clientToken(t, urls[0], id, secret)
	if got := client.TokenKID(tok); got != kid {
		t.Fatalf("A signs with %q, want the issued key %q", got, kid)
	}
	eventually(t, "B accepts a token signed with the key issued on A", func() bool {
		return modelListStatus(t, urls[1], tok) == http.StatusOK
	})
	eventually(t, "B publishes the key in JWKS", func() bool {
		kids, err := b.JWKSKIDs(t)
		return err == nil && kids[kid]
	})
	idB, secretB := newM2MClient(t, b)
	eventually(t, "B signs with the key issued on A", func() bool {
		tokB, code, err := client.FetchClientCredentialsToken(context.Background(), urls[1], idB, secretB)
		return err == nil && code == http.StatusOK && client.TokenKID(tokB) == kid
	})

	if code, _, err := op.InvalidateKeyPairRaw(t, kid); err != nil || code != http.StatusOK {
		t.Fatalf("invalidate on A: %d %v", code, err)
	}
	eventually(t, "B refuses the invalidated key", func() bool {
		return modelListStatus(t, urls[1], tok) == http.StatusUnauthorized
	})
	if code := modelListStatus(t, urls[1], bootTok); code != http.StatusOK {
		t.Fatalf("control: B refuses the token signed before the issue too: %d, want 200", code)
	}

	if code, _, err := op.ReactivateKeyPairRaw(t, kid, time.Now().Add(time.Hour)); err != nil || code != http.StatusOK {
		t.Fatalf("reactivate on A: %d %v", code, err)
	}
	eventually(t, "B accepts the reactivated key", func() bool {
		return modelListStatus(t, urls[1], tok) == http.StatusOK
	})

	if code, _, err := op.DeleteKeyPairRaw(t, kid); err != nil || code != http.StatusOK {
		t.Fatalf("delete on A: %d %v", code, err)
	}
	eventually(t, "B refuses the deleted key", func() bool {
		return modelListStatus(t, urls[1], tok) == http.StatusUnauthorized
	})
	eventually(t, "B drops the deleted key from JWKS", func() bool {
		kids, err := b.JWKSKIDs(t)
		return err == nil && !kids[kid]
	})
	if code := modelListStatus(t, urls[1], bootTok); code != http.StatusOK {
		t.Fatalf("control: B refuses the token signed before the issue too: %d, want 200", code)
	}
}
