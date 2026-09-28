package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("issue: decode %s: %v", b, err)
	}
	if out.KeyId == "" {
		t.Fatalf("issue: empty keyId in %s", b)
	}
	return out.KeyId
}

// currentKey fetches GET /oauth/keys/keypair/current?audience=aud, returning
// the status and, on 200, the decoded keyId.
func (h *callbackHarness) currentKey(t *testing.T, aud string) (int, string) {
	t.Helper()
	code, b := h.keyCall(t, "GET", "/oauth/keys/keypair/current?audience="+aud, "")
	if code != http.StatusOK {
		return code, ""
	}
	var out struct{ KeyId string }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("current: decode %s: %v", b, err)
	}
	return code, out.KeyId
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
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jwks: %d %s", resp.StatusCode, b)
	}
	var set struct{ Keys []struct{ Kid string } }
	if err := json.Unmarshal(b, &set); err != nil {
		t.Fatalf("decode jwks %s: %v", b, err)
	}
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
	if code := h1.authedStatus(t, bootstrapToken(t, key)); code != http.StatusOK {
		t.Fatalf("bootstrap key not yet invalidated: %d, want 200", code)
	}
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
	kids := h2.jwksKIDs(t)
	if kids[kid] {
		t.Fatal("retired key published")
	}
	if !kids[boot2] {
		t.Fatalf("new bootstrap key %s missing from JWKS", boot2)
	}
	if code, got := h2.currentKey(t, "client"); code != http.StatusOK || got != boot2 {
		t.Fatalf("current audience=client on the new node: %d %q, want 200 %s", code, got, boot2)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/oauth/keys/keypair/" + kid + "/invalidate", ""},
		{"DELETE", "/oauth/keys/keypair/" + kid, ""},
		{"POST", "/oauth/keys/keypair/" + kid + "/reactivate", `{"validTo":"` + future + `"}`},
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
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode stored record: %v", err)
	}
	vault, ok := rec["vault"].(map[string]any)
	if !ok {
		t.Fatalf("record has no vault object: %s", raw)
	}
	sealedStr, ok := vault["sealed"].(string)
	if !ok {
		t.Fatalf("vault has no sealed string: %v", vault)
	}
	sealed, err := base64.StdEncoding.DecodeString(sealedStr)
	if err != nil {
		t.Fatalf("decode sealed: %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	vault["sealed"] = base64.StdEncoding.EncodeToString(sealed)
	tampered, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal tampered record: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE kv_store SET value=$1 WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$2`, tampered, kid); err != nil {
		t.Fatal(err)
	}
	h2 := newKeyStackOnUnseeded(t, s, key)

	// A response leaking the sealed bytes, the decryption failure, or the kid
	// would hand an attacker exactly what they'd need next; the 5xx contract
	// (Gate 3) is a generic message plus a ticket UUID, nothing internal.
	forbidden := []string{"sealed", "decryption", kid}

	// /oauth/token must fail closed: the broken key is the selected signer.
	req, _ := http.NewRequest("POST", h2.baseURL+"/api/oauth/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("testclient", "testsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/oauth/token with a broken signer: %d %s, want 500", resp.StatusCode, body)
	}
	var oauthErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &oauthErr); err != nil || oauthErr.Error != "server_error" {
		t.Fatalf(`/oauth/token error body: %s, want {"error":"server_error",...}`, body)
	}
	assertNoLeak(t, "oauth-token", string(body), forbidden)

	req, _ = http.NewRequest("GET", h2.baseURL+"/api/oauth/keys/keypair/current?audience=client", nil)
	req.Header.Set("Authorization", "Bearer "+bootstrapToken(t, key))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("current with a broken signer: %d %s, want 500", resp.StatusCode, body)
	}
	assertNoLeak(t, "keypair-current", string(body), forbidden)
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
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode stored record: %v", err)
	}
	// Rather than grep for PEM/DER markers (which a differently-shaped private
	// key encoding could dodge), try to parse every string value anywhere in
	// the record — decoded both ways a byte blob could be base64-encoded — as
	// a private key. None must succeed, including the sealed vault bytes.
	walkStrings(rec, func(path, val string) {
		for _, dec := range []func(string) ([]byte, error){
			base64.StdEncoding.DecodeString,
			base64.RawURLEncoding.DecodeString,
		} {
			b, err := dec(val)
			if err != nil {
				continue
			}
			if _, err := x509.ParsePKCS8PrivateKey(b); err == nil {
				t.Fatalf("record field %s decodes to a PKCS8 private key", path)
			}
			if _, err := x509.ParsePKCS1PrivateKey(b); err == nil {
				t.Fatalf("record field %s decodes to a PKCS1 private key", path)
			}
		}
	})

	pub, ok := rec["publicKey"].(string)
	if !ok {
		t.Fatalf("record has no publicKey string: %s", raw)
	}
	spki, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		t.Fatalf("decode publicKey: %v", err)
	}
	if _, err := x509.ParsePKIXPublicKey(spki); err != nil {
		t.Fatal("public key not stored in the clear")
	}
}

// walkStrings calls fn(path, value) for every string found anywhere in v (a
// value decoded from JSON: nested maps, slices, or a bare string), path being
// a dotted/indexed breadcrumb for diagnostics.
func walkStrings(v any, fn func(path, value string)) {
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case string:
			fn(path, x)
		case map[string]any:
			for k, vv := range x {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(p, vv)
			}
		case []any:
			for i, vv := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), vv)
			}
		}
	}
	walk("", v)
}

func TestSigningKeys_GRPCFollowsKeyState(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newKeyStackOn(t, newSchedDB(t), genKey(t))
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
	// A not-found lookup still answers with an EntityResponse envelope
	// (Success=false), not a transport error, so an accepted call is a nil
	// error — not merely "the error isn't Unauthenticated".
	if err := call(); err != nil {
		t.Fatalf("token from an issued key refused on gRPC: %v", err)
	}
	if code, b := h.keyCall(t, "POST", "/oauth/keys/keypair/"+kid+"/invalidate", ""); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, b)
	}
	if err := call(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalidated key accepted on gRPC: %v", err)
	}
}
