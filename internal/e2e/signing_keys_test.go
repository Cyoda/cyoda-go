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
	"net/url"
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
// never carries the private key, gRPC honours the same key state as HTTP, the
// signing key signs again once no issued key pair is active, and an
// invalidated key pair verifies through its grace period and no longer.

// keyStack pairs a callbackHarness with the one M2M client keyCall drives the
// real token endpoint with. The client is created through POST
// /clients by the first keyStack on a database, with that stack's own
// self-signed admin token (h.token — see callback_harness_test.go). M2M
// clients are stored in the database, so a later keyStack on the same
// database — a restart — uses the same client. It cannot create one of its
// own: several scenarios build it only AFTER invalidating the very signing
// key its self-signed admin token is signed with.
type keyStack struct {
	*callbackHarness
	clientID, clientSecret string
}

// newKeyStackOn opens a stack on s's database with the given bootstrap key —
// a restart of a node, or a node configured with another key.
func newKeyStackOn(t *testing.T, s *schedDB, key *rsa.PrivateKey) *keyStack {
	t.Helper()
	return newKeyStackWith(t, s, key, nil)
}

// newKeyStackWith is newKeyStackOn with configure applied to the stack's
// config after the database is set; configure may be nil.
func newKeyStackWith(t *testing.T, s *schedDB, key *rsa.PrivateKey, configure func(*app.Config)) *keyStack {
	t.Helper()
	h := newCalloutHarnessWithKey(t, key, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
		if configure != nil {
			configure(cfg)
		}
	})
	if s.keyClient == nil {
		s.keyClient = createKeyStackClient(t, h)
	}
	return &keyStack{callbackHarness: h, clientID: s.keyClient.id, clientSecret: s.keyClient.secret}
}

// createKeyStackClient creates an admin M2M client in the PLATFORM tenant
// through POST /clients?withAdminRole=true, with h's platform-operator token.
// The client lives as long as the test's database.
func createKeyStackClient(t *testing.T, h *callbackHarness) *m2mCredential {
	t.Helper()
	resp := doAuthAgainst(t, h.baseURL, h.platformToken(t), http.MethodPost, "/api/clients?withAdminRole=true", "")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create keyStack M2M client: %d %s", resp.StatusCode, raw)
	}
	cred := decodeCredential(t, "create keyStack M2M client", raw)
	return &cred
}

// oauthToken fetches a fresh bearer through the real token endpoint —
// unlike h.token/h.fetchToken (self-signed), this exercises the server's own
// signer selection, so a caller testing key-rotation/invalidation behaviour
// observes whichever key the server currently signs with.
func (ks *keyStack) oauthToken(t *testing.T) string {
	t.Helper()
	return ks.fetchTokenFor(t, string(auth.PlatformTenantID), ks.clientID, ks.clientSecret)
}

func (ks *keyStack) keyCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ks.baseURL+"/api"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ks.oauthToken(t))
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

func (ks *keyStack) issueKey(t *testing.T, invalidate bool) string {
	t.Helper()
	body := `{"algorithm":"RS256"` + map[bool]string{true: `,"invalidateCurrent":true`, false: ""}[invalidate] + `}`
	code, b := ks.keyCall(t, "POST", "/oauth/keys/keypair", body)
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

// currentKey fetches GET /oauth/keys/keypair/current, returning the status
// and, on 200, the decoded keyId.
func (ks *keyStack) currentKey(t *testing.T) (int, string) {
	t.Helper()
	code, b := ks.keyCall(t, "GET", "/oauth/keys/keypair/current", "")
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

// authedStatus reports the status of GET /api/account with tok: 200 when tok
// authenticates, whatever its roles (the operation needs no role), 401 when
// it does not.
func (h *callbackHarness) authedStatus(t *testing.T, tok string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", h.baseURL+"/api/account", nil)
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

// bootstrapToken signs a token for the signing key itself (the node's
// configured bootstrap key, before any key pair is issued) in the PLATFORM
// tenant, so it is a platform operator — the same principal `cyoda token
// --tenant PLATFORM` models (see operatorToken in cyoda_token_test.go).
func bootstrapToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	tok, err := signServiceToken(key, "cyoda-callback-test", "", "boot-user", string(auth.PlatformTenantID), "boot-user", []string{"ROLE_ADMIN"})
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
	kid := h1.issueKey(t, false)
	h2 := newKeyStackOn(t, s, key)
	tok := h2.oauthToken(t)
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
	h1.issueKey(t, false) // tokens now come from an issued key
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
	kid := h1.issueKey(t, false)
	key2 := genKey(t)
	h2 := newKeyStackOn(t, s, key2)
	boot2, _ := auth.DeriveKID(&key2.PublicKey)
	if got := tokenKID(t, h2.oauthToken(t)); got != boot2 {
		t.Fatalf("node with a new bootstrap key signs with %s, want its bootstrap %s", got, boot2)
	}
	kids := h2.jwksKIDs(t)
	if kids[kid] {
		t.Fatal("retired key published")
	}
	if !kids[boot2] {
		t.Fatalf("new bootstrap key %s missing from JWKS", boot2)
	}
	if code, got := h2.currentKey(t); code != http.StatusOK || got != boot2 {
		t.Fatalf("current on the new node: %d %q, want 200 %s", code, got, boot2)
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
	kid := h1.issueKey(t, false)
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
	h2 := newKeyStackOn(t, s, key)

	// A response leaking the sealed bytes, the decryption failure, or the kid
	// would hand an attacker exactly what they'd need next; the 5xx contract
	// (Gate 3) is a generic message plus a ticket UUID, nothing internal.
	forbidden := []string{"sealed", "decryption", kid}

	// The keyStack's M2M client authenticates via Basic Auth (bcrypt secret
	// check), no JWT needed for that, so it is unaffected by the tampered key.

	// the token endpoint must fail closed: the broken key is the selected signer.
	resp := postTokenTo(t, h2.baseURL, string(auth.PlatformTenantID), url.Values{"grant_type": {"client_credentials"}}, h2.clientID, h2.clientSecret)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("the token endpoint with a broken signer: %d %s, want 500", resp.StatusCode, body)
	}
	var oauthErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &oauthErr); err != nil || oauthErr.Error != "server_error" {
		t.Fatalf(`token endpoint error body: %s, want {"error":"server_error",...}`, body)
	}
	assertNoLeak(t, "oauth-token", string(body), forbidden)

	req, _ := http.NewRequest("GET", h2.baseURL+"/api/oauth/keys/keypair/current", nil)
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
	kid := h.issueKey(t, false)
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
	kid := h.issueKey(t, false)
	tok := h.oauthToken(t)
	if tokenKID(t, tok) != kid {
		t.Fatal("token not signed with the issued key")
	}
	if err := grpcEntitySearch(h, tok); err != nil {
		t.Fatalf("token from an issued key refused on gRPC: %v", err)
	}
	if code, b := h.keyCall(t, "POST", "/oauth/keys/keypair/"+kid+"/invalidate", ""); code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", code, b)
	}
	if err := grpcEntitySearch(h, tok); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalidated key accepted on gRPC: %v", err)
	}
}

// grpcEntitySearch runs one unary gRPC call (EntitySearch for an absent
// entity) under tok. A not-found lookup still answers with an EntityResponse
// envelope (Success=false), not a transport error, so an accepted call
// returns nil — not merely "an error other than Unauthenticated"; a refused
// token returns Unauthenticated.
func grpcEntitySearch(h *keyStack, tok string) error {
	ce, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{"id": "sk", "entityId": "00000000-0000-0000-0000-000000000000"})
	if err != nil {
		return err
	}
	_, err = cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(h.grpcCtxAs(tok, ""), ce)
	return err
}

// invalidateKey invalidates the key pair kid on ks with the given grace period.
func (ks *keyStack) invalidateKey(t *testing.T, kid string, grace time.Duration) {
	t.Helper()
	body := fmt.Sprintf(`{"gracePeriodSec":%d}`, int64(grace/time.Second))
	if code, b := ks.keyCall(t, "POST", "/oauth/keys/keypair/"+kid+"/invalidate", body); code != http.StatusOK {
		t.Fatalf("invalidate %s: %d %s", kid, code, b)
	}
}

// TestSigningKeys_SigningKeySignsWhenNoIssuedPairIsActive: once no issued key
// pair is active and in its window — here one invalidated and one deleted —
// the signing key from configuration is the current key pair and
// the token endpoint signs with it.
func TestSigningKeys_SigningKeySignsWhenNoIssuedPairIsActive(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	key := genKey(t)
	bootKID, err := auth.DeriveKID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newKeyStackOn(t, newSchedDB(t), key)
	k1 := h.issueKey(t, false)
	k2 := h.issueKey(t, false)
	h.invalidateKey(t, k2, 0)
	if code, cur := h.currentKey(t); code != http.StatusOK || cur != k1 {
		t.Fatalf("control: current with k2 invalidated: %d %s, want 200 %s", code, cur, k1)
	}
	if code, b := h.keyCall(t, "DELETE", "/oauth/keys/keypair/"+k1, ""); code != http.StatusOK {
		t.Fatalf("delete %s: %d %s", k1, code, b)
	}
	if code, cur := h.currentKey(t); code != http.StatusOK || cur != bootKID {
		t.Fatalf("current: %d %s, want 200 and the signing key %s", code, cur, bootKID)
	}
	tok := h.oauthToken(t)
	if got := tokenKID(t, tok); got != bootKID {
		t.Fatalf("the token endpoint signs with %s, want the signing key %s", got, bootKID)
	}
	if code := h.authedStatus(t, tok); code != http.StatusOK {
		t.Fatalf("token signed by the signing key: %d, want 200", code)
	}
}

// issueSigning issues a key pair on ks and fetches a token it signs: the
// newest active key pair is the signer.
func (ks *keyStack) issueSigning(t *testing.T) (kid, tok string) {
	t.Helper()
	kid = ks.issueKey(t, false)
	tok = ks.oauthToken(t)
	if got := tokenKID(t, tok); got != kid {
		t.Fatalf("the server signs with %s, want the key pair just issued %s", got, kid)
	}
	return kid, tok
}

// TestSigningKeys_InvalidatedKeyPairEndsEarly covers the cases that need no
// time to pass: grace 0 refuses at once; during a grace period, invalidating
// again with 0 or DELETE refuses at once; and a key pair in its grace period
// verifies but is never the signer. The grace period is an hour, so nothing
// here depends on timing.
func TestSigningKeys_InvalidatedKeyPairEndsEarly(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const grace = time.Hour
	h := newKeyStackOn(t, newSchedDB(t), genKey(t))
	kZero, tZero := h.issueSigning(t) // invalidated with grace 0
	kCut, tCut := h.issueSigning(t)   // grace cut short by an invalidate with 0
	kDel, tDel := h.issueSigning(t)   // grace cut short by DELETE
	kAdmin := h.issueKey(t, false)
	kLong, tLong := h.issueSigning(t) // the newest, in grace: verifies, never signs

	h.invalidateKey(t, kZero, 0)
	if code := h.authedStatus(t, tZero); code != http.StatusUnauthorized {
		t.Errorf("invalidated with grace 0: %d, want 401", code)
	}

	h.invalidateKey(t, kLong, grace)
	if code := h.authedStatus(t, tLong); code != http.StatusOK {
		t.Errorf("in its grace period: %d, want 200", code)
	}
	if code, cur := h.currentKey(t); code != http.StatusOK || cur != kAdmin {
		t.Errorf("current: %d %s, want 200 %s (a key pair in grace never signs)", code, cur, kAdmin)
	}

	h.invalidateKey(t, kCut, grace)
	if code := h.authedStatus(t, tCut); code != http.StatusOK {
		t.Fatalf("control: in its grace period: %d, want 200", code)
	}
	h.invalidateKey(t, kCut, 0)
	if code := h.authedStatus(t, tCut); code != http.StatusUnauthorized {
		t.Errorf("after invalidating again with grace 0: %d, want 401", code)
	}

	h.invalidateKey(t, kDel, grace)
	if code := h.authedStatus(t, tDel); code != http.StatusOK {
		t.Fatalf("control: in its grace period: %d, want 200", code)
	}
	if code, b := h.keyCall(t, "DELETE", "/oauth/keys/keypair/"+kDel, ""); code != http.StatusOK {
		t.Fatalf("delete %s: %d %s", kDel, code, b)
	}
	if code := h.authedStatus(t, tDel); code != http.StatusUnauthorized {
		t.Errorf("after DELETE during the grace period: %d, want 401", code)
	}
}

// TestSigningKeys_GracePeriod: an invalidated key pair — issued, or the
// signing key from configuration — verifies until the validTo its grace
// period sets and is refused after it; one reactivated during its grace
// period is active again and outlives it. The issued key pair's token is
// checked over HTTP and over gRPC, which authenticate separately.
//
// The in-grace checks must finish before the earliest validTo the
// invalidations can have set, or the test fails rather than passing on
// nothing; they are kept to two HTTP reads, one unary gRPC call and one
// reactivate. The after-grace checks run a margin after the latest validTo.
func TestSigningKeys_GracePeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const grace = 10 * time.Second
	key := genKey(t)
	bootKID, err := auth.DeriveKID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newKeyStackOn(t, newSchedDB(t), key)
	opTok := operatorToken(t, key, "cyoda-callback-test", "")
	kReact, tReact := h.issueSigning(t) // reactivated during its grace
	h.issueKey(t, false)                // signs the admin calls once the others end
	kGrace, tGrace := h.issueSigning(t) // grace runs out

	earliestValidTo := time.Now().Add(grace)
	h.invalidateKey(t, kGrace, grace)
	h.invalidateKey(t, bootKID, grace)
	h.invalidateKey(t, kReact, grace)
	latestValidTo := time.Now().Add(grace)
	if code := h.authedStatus(t, tGrace); code != http.StatusOK {
		t.Errorf("inside the grace period, issued key pair: %d, want 200", code)
	}
	if code := h.authedStatus(t, opTok); code != http.StatusOK {
		t.Errorf("inside the grace period, signing key: %d, want 200", code)
	}
	if err := grpcEntitySearch(h, tGrace); err != nil {
		t.Errorf("inside the grace period, issued key pair on gRPC: %v, want accepted", err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, b := h.keyCall(t, "POST", "/oauth/keys/keypair/"+kReact+"/reactivate", `{"validTo":"`+future+`"}`); code != http.StatusOK {
		t.Fatalf("reactivate %s: %d %s", kReact, code, b)
	}
	if now := time.Now(); !now.Before(earliestValidTo) {
		t.Fatalf("the in-grace steps ended %s after the earliest end of the grace period; they prove nothing", now.Sub(earliestValidTo))
	}
	if code, cur := h.currentKey(t); code != http.StatusOK || cur != kReact {
		t.Errorf("current after the reactivate: %d %s, want 200 %s", code, cur, kReact)
	}

	time.Sleep(time.Until(latestValidTo.Add(time.Second)))
	for _, c := range []struct {
		name, tok string
		want      int
	}{
		{"issued key pair", tGrace, http.StatusUnauthorized},
		{"signing key", opTok, http.StatusUnauthorized},
		{"reactivated key pair", tReact, http.StatusOK},
	} {
		if code := h.authedStatus(t, c.tok); code != c.want {
			t.Errorf("after the grace period, %s: %d, want %d", c.name, code, c.want)
		}
	}
	if err := grpcEntitySearch(h, tGrace); status.Code(err) != codes.Unauthenticated {
		t.Errorf("after the grace period, issued key pair on gRPC: %v, want Unauthenticated", err)
	}
}
