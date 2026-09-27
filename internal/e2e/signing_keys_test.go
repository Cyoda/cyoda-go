package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// signing_keys_test.go proves the cluster-shared, persisted JWT signing key
// pairs end to end against a real PostgreSQL: an issued key survives a
// restart, a bootstrap-key revocation survives a restart, a new bootstrap key
// retires the issued pairs it did not mint, a broken (tampered) signer fails
// closed rather than silently substituting another key, the stored KV record
// never carries the private key, and gRPC honours the same key state as HTTP.
//
// newKeyStackOn opens a stack on s's database with the given bootstrap key —
// a restart of a node, or a node configured with another key.
func newKeyStackOn(t *testing.T, s *schedDB, key *rsa.PrivateKey) *callbackHarness {
	t.Helper()
	return newCalloutHarnessWithKey(t, key, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
	})
}

// newKeyStackOnUnseeded is newKeyStackOn without seeding the cached bearer
// token — for a stack whose signer is expected to be broken, where seeding
// the token would itself fail.
func newKeyStackOnUnseeded(t *testing.T, s *schedDB, key *rsa.PrivateKey) *callbackHarness {
	t.Helper()
	return newCalloutHarnessUnseeded(t, key, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
	})
}

func (h *callbackHarness) keyCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.baseURL+"/api"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.fetchToken(t))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *callbackHarness) issueKey(t *testing.T, aud string, invalidate bool) string {
	t.Helper()
	body := `{"algorithm":"RS256","audience":"` + aud + `"` + map[bool]string{true: `,"invalidateCurrent":true`, false: ""}[invalidate] + `}`
	code, b := h.keyCall(t, "POST", "/oauth/keys/keypair", body)
	if code != http.StatusOK {
		t.Fatalf("issue: %d %s", code, b)
	}
	var out struct{ KeyId string }
	_ = json.Unmarshal(b, &out)
	return out.KeyId
}

func tokenKID(t *testing.T, tok string) string {
	t.Helper()
	p, err := auth.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := p.Header["kid"].(string)
	return kid
}

func (h *callbackHarness) authedStatus(t *testing.T, tok string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", h.baseURL+"/api/model/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (h *callbackHarness) jwksKIDs(t *testing.T) map[string]bool {
	t.Helper()
	resp, err := http.Get(h.baseURL + "/api/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var set struct{ Keys []struct{ Kid string } }
	_ = json.NewDecoder(resp.Body).Decode(&set)
	out := map[string]bool{}
	for _, k := range set.Keys {
		out[k.Kid] = true
	}
	return out
}

func bootstrapToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	kid, _ := auth.DeriveKID(&key.PublicKey)
	now := time.Now()
	tok, err := auth.Sign(context.Background(), map[string]any{
		"sub": "boot-user", "iss": "cyoda-callback-test", "caas_user_id": "boot-user",
		"caas_org_id": "test-tenant", "scopes": []string{"ROLE_ADMIN"}, "caas_tier": "unlimited",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "jti": "j-" + kid[:8],
	}, auth.NewRSASigner(key), kid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSigningKeys_IssuedPairSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	h1 := newKeyStackOn(t, s, key)
	kid := h1.issueKey(t, "client", false)
	h2 := newKeyStackOn(t, s, key)
	tok := h2.fetchToken(t)
	if tokenKID(t, tok) != kid {
		t.Fatalf("restarted node signs with %s, want the issued %s", tokenKID(t, tok), kid)
	}
	if code := h2.authedStatus(t, tok); code != http.StatusOK {
		t.Fatalf("token from the issued key: %d", code)
	}
	if !h2.jwksKIDs(t)[kid] {
		t.Fatal("issued key missing from JWKS after restart")
	}
}

func TestSigningKeys_BootstrapRevocationSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	bootKID, _ := auth.DeriveKID(&key.PublicKey)
	h1 := newKeyStackOn(t, s, key)
	h1.issueKey(t, "client", false) // tokens now come from an issued key
	if code, b := h1.keyCall(t, "POST", "/oauth/keys/keypair/"+bootKID+"/invalidate", ""); code != http.StatusOK {
		t.Fatalf("invalidate bootstrap: %d %s", code, b)
	}
	h2 := newKeyStackOn(t, s, key)
	if code := h2.authedStatus(t, bootstrapToken(t, key)); code != http.StatusUnauthorized {
		t.Fatalf("invalidated bootstrap key accepted after restart: %d", code)
	}
	if code, b := h2.keyCall(t, "DELETE", "/oauth/keys/keypair/"+bootKID, ""); code != http.StatusOK {
		t.Fatalf("delete bootstrap: %d %s", code, b)
	}
	h3 := newKeyStackOn(t, s, key)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, _ := h3.keyCall(t, "POST", "/oauth/keys/keypair/"+bootKID+"/reactivate", `{"validTo":"`+future+`"}`); code != http.StatusNotFound {
		t.Fatalf("reactivate after delete: %d, want 404", code)
	}
	if code := h3.authedStatus(t, bootstrapToken(t, key)); code != http.StatusUnauthorized {
		t.Fatalf("deleted bootstrap key accepted after restart: %d", code)
	}
}

func TestSigningKeys_AnotherBootstrapKeyRetiresIssuedPairs(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s := newSchedDB(t)
	h1 := newKeyStackOn(t, s, genKey(t))
	kid := h1.issueKey(t, "client", false)
	key2 := genKey(t)
	h2 := newKeyStackOn(t, s, key2)
	boot2, _ := auth.DeriveKID(&key2.PublicKey)
	if got := tokenKID(t, h2.fetchToken(t)); got != boot2 {
		t.Fatalf("node with a new bootstrap key signs with %s, want its bootstrap %s", got, boot2)
	}
	if h2.jwksKIDs(t)[kid] {
		t.Fatal("retired key published")
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/oauth/keys/keypair/" + kid + "/invalidate", ""},
		{"DELETE", "/oauth/keys/keypair/" + kid, ""},
	} {
		if code, _ := h2.keyCall(t, c.method, c.path, c.body); code != http.StatusNotFound {
			t.Fatalf("%s %s on a retired key: %d, want 404", c.method, c.path, code)
		}
	}
}

func TestSigningKeys_BrokenSignerFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	h1 := newKeyStackOn(t, s, key)
	kid := h1.issueKey(t, "client", false)
	var raw []byte
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, `SELECT value FROM kv_store WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$1`, kid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	_ = json.Unmarshal(raw, &rec)
	vault := rec["vault"].(map[string]any)
	sealed, _ := base64.StdEncoding.DecodeString(vault["sealed"].(string))
	sealed[len(sealed)-1] ^= 1
	vault["sealed"] = base64.StdEncoding.EncodeToString(sealed)
	tampered, _ := json.Marshal(rec)
	if _, err := s.pool.Exec(ctx, `UPDATE kv_store SET value=$1 WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$2`, tampered, kid); err != nil {
		t.Fatal(err)
	}
	h2 := newKeyStackOnUnseeded(t, s, key)
	// /oauth/token must fail closed: the broken key is the selected signer.
	req, _ := http.NewRequest("POST", h2.baseURL+"/api/oauth/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("testclient", "testsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/oauth/token with a broken signer: %d, want 500", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", h2.baseURL+"/api/oauth/keys/keypair/current?audience=client", nil)
	req.Header.Set("Authorization", "Bearer "+bootstrapToken(t, key))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("current with a broken signer: %d, want 500", resp.StatusCode)
	}
}

func TestSigningKeys_StoredValueHasNoPrivateKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	h := newKeyStackOn(t, s, key)
	kid := h.issueKey(t, "client", false)
	var raw []byte
	if err := s.pool.QueryRow(context.Background(), `SELECT value FROM kv_store WHERE namespace='signing-keys' AND key=$1`, kid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"PRIVATE KEY", "MIIE"} {
		if strings.Contains(string(raw), marker) {
			t.Fatalf("stored record contains %q", marker)
		}
	}
	var rec struct{ PublicKey string }
	_ = json.Unmarshal(raw, &rec)
	spki, _ := base64.StdEncoding.DecodeString(rec.PublicKey)
	if _, err := x509.ParsePKIXPublicKey(spki); err != nil {
		t.Fatal("public key not stored in the clear")
	}
}

func TestSigningKeys_GRPCFollowsKeyState(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newCalloutHarness(t, nil)
	kid := h.issueKey(t, "client", false)
	tok := h.fetchToken(t)
	if tokenKID(t, tok) != kid {
		t.Fatal("token not signed with the issued key")
	}
	call := func() error {
		ce, _ := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{"id": "sk", "entityId": "00000000-0000-0000-0000-000000000000"})
		_, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(h.grpcCtxAs(tok, ""), ce)
		return err
	}
	if err := call(); status.Code(err) == codes.Unauthenticated {
		t.Fatalf("token from an issued key refused on gRPC: %v", err)
	}
	if code, b := h.keyCall(t, "POST", "/oauth/keys/keypair/"+kid+"/invalidate", ""); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, b)
	}
	if err := call(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalidated key accepted on gRPC: %v", err)
	}
}
