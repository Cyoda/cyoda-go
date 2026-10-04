package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// The token-exchange grant and the subject token type it accepts.
const (
	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	jwtTokenType       = "urn:ietf:params:oauth:token-type:jwt"
)

// oboSigner is the on-behalf-of client and the trusted assertion-signing key
// one tenant of one server uses for every oboToken of the run.
type oboSigner struct {
	clientID, secret string
	key              *rsa.PrivateKey
	kid              string
}

// oboSigners caches one oboSigner per (server, tenant, admin signing key).
// The signing key is part of the key because a per-test stack signs its
// admin tokens with a key of its own and can reuse a port a finished stack
// held. On the shared server the client and the trusted key are never
// deleted: the tenant holds one of each for the run, far under its caps. A
// harness stack's signer belongs to the test that asked for it — the test's
// name is part of the key — and is deleted when that test ends: every stack
// of the package stores into the one database, under the same tenant as the
// shared server, so a key left behind by each stack would fill the tenant's
// trusted-key cap.
var oboSigners = struct {
	sync.Mutex
	m map[string]*oboSigner
}{m: map[string]*oboSigner{}}

// oboToken is an on-behalf-of token of the suite tenant for user, on the
// shared server.
func oboToken(t *testing.T, user string) string {
	t.Helper()
	return oboTokenFor(t, suiteToken(t), user)
}

// oboTokenFor is an on-behalf-of token for user in the tenant of adminToken,
// a tenant admin's token for the shared server.
func oboTokenFor(t *testing.T, adminToken, user string) string {
	t.Helper()
	return oboTokenOn(t, serverURL, adminToken, user)
}

// oboTokenOn is oboTokenFor against the server at baseURL — the shared
// server or a harness stack. On the first call for a tenant it creates an
// OBO client (POST /api/clients?onBehalfOf=true) and registers a trusted key
// (POST /api/oauth/keys/trusted) as adminToken; then it signs an assertion
// for user — aud the server's issuer, read from adminToken's iss — and
// exchanges it. Any failure fails the test.
func oboTokenOn(t *testing.T, baseURL, adminToken, user string) string {
	t.Helper()
	claims := decodeJWTPayload(t, adminToken)
	tenant, _ := claims["caas_org_id"].(string)
	issuer, _ := claims["iss"].(string)
	if tenant == "" || issuer == "" {
		t.Fatal("oboTokenOn: admin token carries no caas_org_id or iss")
	}
	s := oboSignerFor(t, baseURL, adminToken, tenant)

	now := time.Now()
	form := url.Values{"subject_token": {signAssertion(s.key, s.kid, map[string]any{
		"sub": user, "iss": "e2e-app", "aud": issuer, "caas_org_id": tenant,
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": uuid.NewString(),
	})}}
	resp, err := exchangeRawTo(e2eCtx(t), baseURL, tenant, s.clientID, s.secret, form)
	if err != nil {
		t.Fatalf("oboTokenOn: exchange: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("oboTokenOn: exchange: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		t.Fatalf("oboTokenOn: no access_token in the exchange response (%v)", err)
	}
	return out.AccessToken
}

// oboClientOf is the id of the on-behalf-of client an OBO token was issued
// to: its act.sub, the executor every write the token makes records.
func oboClientOf(t *testing.T, oboTok string) string {
	t.Helper()
	act, _ := decodeJWTPayload(t, oboTok)["act"].(map[string]any)
	id, _ := act["sub"].(string)
	if id == "" {
		t.Fatal("oboClientOf: the token carries no act.sub")
	}
	return id
}

// oboSignerFor returns the cached oboSigner of tenant on baseURL, creating
// it as adminToken on first use.
func oboSignerFor(t *testing.T, baseURL, adminToken, tenant string) *oboSigner {
	t.Helper()
	parsed, err := auth.Parse(adminToken)
	if err != nil {
		t.Fatalf("oboSignerFor: parse admin token: %v", err)
	}
	adminKID, _ := parsed.Header["kid"].(string)
	cacheKey := baseURL + "|" + tenant + "|" + adminKID
	testOwned := baseURL != serverURL
	if testOwned {
		cacheKey += "|" + t.Name()
	}

	oboSigners.Lock()
	defer oboSigners.Unlock()
	if s, ok := oboSigners.m[cacheKey]; ok {
		return s
	}

	resp := doAuthAgainst(t, baseURL, adminToken, http.MethodPost, "/api/clients?onBehalfOf=true", "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("oboSignerFor: create OBO client: %d %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	cred := decodeCredential(t, "oboSignerFor: create OBO client", raw)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("oboSignerFor: generate key: %v", err)
	}
	kid := "e2e-obo-" + uuid.NewString()[:8]
	body, _ := json.Marshal(trustedKeyBody(key, kid))
	reg := doAuthAgainst(t, baseURL, adminToken, http.MethodPost, "/api/oauth/keys/trusted", string(body))
	regRaw, _ := io.ReadAll(reg.Body)
	reg.Body.Close()
	if reg.StatusCode != http.StatusOK {
		t.Fatalf("oboSignerFor: register trusted key: %d %s", reg.StatusCode, regRaw)
	}

	s := &oboSigner{clientID: cred.id, secret: cred.secret, key: key, kid: kid}
	oboSigners.m[cacheKey] = s
	if testOwned {
		// Registered after the stack's own cleanups, so these run first,
		// while the stack still serves.
		deleteClientAtCleanup(t, baseURL, cred.id, func() string { return adminToken })
		t.Cleanup(func() {
			del := doAuthAgainst(t, baseURL, adminToken, http.MethodDelete, "/api/oauth/keys/trusted/"+kid, "")
			delRaw, _ := io.ReadAll(del.Body)
			del.Body.Close()
			if del.StatusCode != http.StatusOK {
				t.Errorf("cleanup: delete trusted key %s: %d %s", kid, del.StatusCode, delRaw)
			}
			oboSigners.Lock()
			defer oboSigners.Unlock()
			delete(oboSigners.m, cacheKey)
		})
	}
	return s
}

// signAssertion signs claims as an RS256 user assertion with key under kid,
// as an application signs one. It never touches *testing.T; it panics only
// if signing with a freshly generated RSA key fails.
func signAssertion(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	tok, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)
	if err != nil {
		panic(fmt.Sprintf("signAssertion: %v", err))
	}
	return tok
}

// exchangeRaw runs the token-exchange grant on the shared server as client
// clientID with form. grant_type is set, and subject_token_type defaults to
// the JWT type when form does not carry the key. It never touches
// *testing.T, so it is safe to call from a goroutine.
func exchangeRaw(tenant, clientID, secret string, form url.Values) (*http.Response, error) {
	return exchangeRawTo(context.Background(), serverURL, tenant, clientID, secret, form)
}

// exchangeRawTo is exchangeRaw against the server at baseURL.
func exchangeRawTo(ctx context.Context, baseURL, tenant, clientID, secret string, form url.Values) (*http.Response, error) {
	f := url.Values{}
	for k, v := range form {
		f[k] = append([]string(nil), v...)
	}
	f.Set("grant_type", tokenExchangeGrant)
	if _, set := f["subject_token_type"]; !set {
		f.Set("subject_token_type", jwtTokenType)
	}
	return postTokenRaw(ctx, baseURL, tenant, f, clientID, secret)
}

// trustedKeyBody is the POST /oauth/keys/trusted body registering priv's
// public half under kid.
func trustedKeyBody(priv *rsa.PrivateKey, kid string) map[string]any {
	return map[string]any{"keyId": kid, "jwk": map[string]any{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes()),
	}}
}

// registerTrustedSignerAs generates an RSA key and registers its public half
// as a trusted key of adminToken's tenant on the shared server, under a fresh
// kid unless kid is given, with extra merged into the request body. The key
// is deleted when the test ends (a tenant's trusted-key cap is shared by
// every test in the run).
func registerTrustedSignerAs(t *testing.T, adminToken, kid string, extra map[string]any) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	if kid == "" {
		kid = "e2e-tx-" + uuid.NewString()[:8]
	}
	body := trustedKeyBody(priv, kid)
	for k, v := range extra {
		body[k] = v
	}
	resp := requestAs(t, adminToken, http.MethodPost, "/oauth/keys/trusted", mustJSON(t, body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register trusted key: status=%d, want 200; body: %s", resp.StatusCode, raw)
	}
	t.Cleanup(func() {
		del := requestAs(t, adminToken, http.MethodDelete, "/oauth/keys/trusted/"+kid, nil)
		defer del.Body.Close()
		if del.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(del.Body)
			t.Errorf("delete trusted key %s: status=%d; body: %s", kid, del.StatusCode, raw)
		}
	})
	return priv, kid
}

// assertionFor is a valid assertion for sub in tenant, for the shared
// server's issuer, signed with priv under kid; mutate edits the claims before
// signing and may be nil.
func assertionFor(priv *rsa.PrivateKey, kid, sub, tenant string, mutate func(map[string]any)) string {
	now := time.Now()
	c := map[string]any{
		"sub": sub, "iss": "e2e-app", "aud": e2eIssuer, "caas_org_id": tenant,
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": uuid.NewString(),
	}
	if mutate != nil {
		mutate(c)
	}
	return signAssertion(priv, kid, c)
}

// assertOAuthErrorDesc is assertOAuthError that also pins error_description.
func assertOAuthErrorDesc(t *testing.T, resp *http.Response, wantStatus int, wantErr, wantDesc string) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	if resp.StatusCode != wantStatus || e.Error != wantErr || e.ErrorDescription != wantDesc {
		t.Fatalf("got %d %s, want %d %s / %s", resp.StatusCode, strings.TrimSpace(string(raw)), wantStatus, wantErr, wantDesc)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("content-type %q, want application/json", resp.Header.Get("Content-Type"))
	}
}

// assertOBORefusedAdmin checks the 403 FORBIDDEN an on-behalf-of token gets
// on an administration route, with the detail that names the refusal, which
// tells it apart from the 403 a principal without ROLE_ADMIN gets.
func assertOBORefusedAdmin(t *testing.T, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("on-behalf-of token: %d, want 403: %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	if code := problemErrorCode(string(raw)); code != "FORBIDDEN" {
		t.Errorf("errorCode %q, want FORBIDDEN: %s", code, raw)
	}
	if d := problemDetail(t, string(raw)); !strings.Contains(d, "on-behalf-of tokens cannot administer") {
		t.Errorf("detail %q, want it to name on-behalf-of tokens", d)
	}
}
