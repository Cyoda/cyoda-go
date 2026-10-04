package e2e_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The fixed descriptions of the token endpoint's refusals.
const (
	descOnlyExchange = "this client may only exchange user assertions"
	descNoExchange   = "this client may not exchange user assertions"
	descParam        = "unsupported parameter"
	descType         = "unsupported subject_token_type"
	descInvalid      = "invalid subject token"
	descKey          = "unknown or inactive trusted key"
	descSig          = "subject token signature or issuer rejected"
	descClaims       = "subject token claims rejected"
	descTenant       = "tenant mismatch"
	descSub          = "subject token sub rejected"
	descExpired      = "subject token has expired"
)

// exchangeAs runs the token exchange on the shared server as the suite-tenant
// client clientID for a
// subject_token, with extra form fields as key/value pairs.
func exchangeAs(t *testing.T, clientID, secret, subject string, extra ...string) *http.Response {
	t.Helper()
	return exchangeAsIn(t, suiteTenant, clientID, secret, subject, extra...)
}

// exchangeAsIn is exchangeAs for a client of tenant.
func exchangeAsIn(t *testing.T, tenant, clientID, secret, subject string, extra ...string) *http.Response {
	t.Helper()
	form := url.Values{"subject_token": {subject}}
	for i := 0; i+1 < len(extra); i += 2 {
		form.Set(extra[i], extra[i+1])
	}
	resp, err := exchangeRaw(tenant, clientID, secret, form)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	return resp
}

// TestToken_TokenExchange_Refusals: every §12.1 refusal of the exchange a
// request can trigger, on the running server — status, error code and the
// fixed description, which never echoes the tenant or the key id.
func TestToken_TokenExchange_Refusals(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const tenant = "test-tenant"
	oboID, oboSecret := createClient(t, false, true)
	plainID, plainSecret := createClient(t, false, false)
	priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", nil)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a := func(mutate func(map[string]any)) string { return assertionFor(priv, kid, "alice", tenant, mutate) }
	ok := a(nil)
	now := time.Now()
	valid := map[string]any{"sub": "alice", "aud": e2eIssuer, "caas_org_id": tenant,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}

	type tc struct {
		name       string
		plain      bool
		subject    string
		extra      []string
		status     int
		code, desc string
	}
	cases := []tc{
		{name: "non-OBO client", plain: true, subject: ok, status: 400, code: "unauthorized_client", desc: descNoExchange},
		// The client check precedes the parameter check.
		{name: "non-OBO client, actor_token", plain: true, subject: ok, extra: []string{"actor_token", "x"}, status: 400, code: "unauthorized_client", desc: descNoExchange},
		{name: "actor_token", subject: ok, extra: []string{"actor_token", "x"}, status: 400, code: "invalid_request", desc: descParam},
		{name: "actor_token_type", subject: ok, extra: []string{"actor_token_type", "x"}, status: 400, code: "invalid_request", desc: descParam},
		{name: "resource", subject: ok, extra: []string{"resource", "x"}, status: 400, code: "invalid_request", desc: descParam},
		{name: "audience", subject: ok, extra: []string{"audience", "x"}, status: 400, code: "invalid_request", desc: descParam},
		{name: "empty scope", subject: ok, extra: []string{"scope", ""}, status: 400, code: "invalid_request", desc: descParam},
		{name: "requested_token_type", subject: ok, extra: []string{"requested_token_type", "x"}, status: 400, code: "invalid_request", desc: descParam},
		{name: "access_token type", subject: ok, extra: []string{"subject_token_type", "urn:ietf:params:oauth:token-type:access_token"}, status: 400, code: "invalid_request", desc: descType},
		{name: "malformed", subject: "not-a-jwt", status: 400, code: "invalid_request", desc: descInvalid},
		{name: "alg HS256", subject: signRawHeader(t, priv, map[string]any{"alg": "HS256", "kid": kid}, valid), status: 400, code: "invalid_request", desc: descInvalid},
		{name: "no kid", subject: signAssertion(priv, "", valid), status: 400, code: "invalid_request", desc: descInvalid},
		{name: "unknown kid", subject: signAssertion(priv, "e2e-unknown-"+uuid.NewString()[:8], valid), status: 400, code: "invalid_request", desc: descKey},
		{name: "wrong key", subject: signAssertion(other, kid, valid), status: 400, code: "invalid_request", desc: descSig},
		{name: "aud missing", subject: a(func(c map[string]any) { delete(c, "aud") }), status: 400, code: "invalid_request", desc: descClaims},
		{name: "aud other", subject: a(func(c map[string]any) { c["aud"] = "elsewhere" }), status: 400, code: "invalid_request", desc: descClaims},
		{name: "no exp", subject: a(func(c map[string]any) { delete(c, "exp") }), status: 400, code: "invalid_request", desc: descClaims},
		{name: "no iat", subject: a(func(c map[string]any) { delete(c, "iat") }), status: 400, code: "invalid_request", desc: descClaims},
		{name: "lifetime over 300 s", subject: a(func(c map[string]any) { c["exp"] = time.Now().Add(301 * time.Second).Unix() }), status: 400, code: "invalid_request", desc: descClaims},
		{name: "iat future", subject: a(func(c map[string]any) {
			c["iat"] = time.Now().Add(time.Minute).Unix()
			c["exp"] = time.Now().Add(2 * time.Minute).Unix()
		}), status: 400, code: "invalid_request", desc: descClaims},
		{name: "expired", subject: a(func(c map[string]any) {
			c["iat"] = time.Now().Add(-5 * time.Minute).Unix()
			c["exp"] = time.Now().Add(-time.Minute).Unix()
		}), status: 400, code: "invalid_request", desc: descClaims},
		{name: "nbf future", subject: a(func(c map[string]any) { c["nbf"] = time.Now().Add(time.Minute).Unix() }), status: 400, code: "invalid_request", desc: descClaims},
		{name: "tenant mismatch", subject: a(func(c map[string]any) { c["caas_org_id"] = "other-tenant" }), status: 403, code: "access_denied", desc: descTenant},
		{name: "sub reserved", subject: a(func(c map[string]any) { c["sub"] = "SYSTEM" }), status: 400, code: "invalid_request", desc: descSub},
		{name: "sub newline", subject: a(func(c map[string]any) { c["sub"] = "ext\ninjected" }), status: 400, code: "invalid_request", desc: descSub},
		{name: "sub too long", subject: a(func(c map[string]any) { c["sub"] = strings.Repeat("u", 256) }), status: 400, code: "invalid_request", desc: descSub},
		{name: "sub empty", subject: a(func(c map[string]any) { c["sub"] = "" }), status: 400, code: "invalid_request", desc: descSub},
		{name: "capped exp not in the future", subject: a(func(c map[string]any) {
			c["iat"] = time.Now().Add(-time.Minute).Unix()
			c["exp"] = time.Now().Add(-5 * time.Second).Unix()
		}), status: 400, code: "invalid_request", desc: descExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, sec := oboID, oboSecret
			if c.plain {
				id, sec = plainID, plainSecret
			}
			resp := exchangeAs(t, id, sec, c.subject, c.extra...)
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(raw), tenant) || strings.Contains(string(raw), kid) ||
				strings.Contains(string(raw), "injected") || strings.Contains(string(raw), "uuuu") {
				t.Fatalf("response echoes the tenant, kid or sub: %s", raw)
			}
			resp.Body = io.NopCloser(strings.NewReader(string(raw)))
			assertOAuthErrorDesc(t, resp, c.status, c.code, c.desc)
		})
	}
}

// signRawHeader is an assertion for claims signed with priv whose header is
// replaced by header. The signature no longer matches the header; the
// algorithm and kid checks run before the signature check, so a case that
// lies about either is refused for that reason.
func signRawHeader(t *testing.T, priv *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	parts := strings.SplitN(signAssertion(priv, "", claims), ".", 3)
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(h) + "." + parts[1] + "." + parts[2]
}

// TestToken_TokenExchange_Accepted: an OBO client exchanges an assertion for
// alice; the token carries alice, the client as act.sub, the client's roles
// only (the assertion's ROLE_ADMIN never reaches it), no cgen, and an
// expires_in that is its remaining life — and the server accepts it.
func TestToken_TokenExchange_Accepted(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	oboID, oboSecret := createClient(t, false, true)
	priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", nil)
	subject := assertionFor(priv, kid, "alice", "test-tenant", func(c map[string]any) {
		c["user_roles"] = []string{"ROLE_ADMIN"}
		c["roles"] = []string{"ROLE_ADMIN"}
	})
	resp := exchangeAs(t, oboID, oboSecret, subject)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body: %s", resp.StatusCode, raw)
	}
	var tok struct {
		AccessToken     string  `json:"access_token"`
		TokenType       string  `json:"token_type"`
		ExpiresIn       float64 `json:"expires_in"`
		IssuedTokenType string  `json:"issued_token_type"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("no access_token in the response (err %v)", err)
	}
	if tok.TokenType != "Bearer" || tok.IssuedTokenType != jwtTokenType {
		t.Errorf("token_type %q issued_token_type %q", tok.TokenType, tok.IssuedTokenType)
	}
	claims := decodeJWTPayload(t, tok.AccessToken)
	for k, want := range map[string]any{"sub": "alice", "caas_user_id": "alice", "caas_org_id": "test-tenant", "iss": e2eIssuer} {
		if claims[k] != want {
			t.Errorf("%s = %v, want %v", k, claims[k], want)
		}
	}
	if act, _ := claims["act"].(map[string]any); len(act) != 1 || act["sub"] != oboID {
		t.Errorf("act = %v, want exactly {sub: %s}", claims["act"], oboID)
	}
	if scopes, _ := claims["scopes"].([]any); len(scopes) != 1 || scopes[0] != "ROLE_M2M" {
		t.Errorf("scopes = %v, want the client's [ROLE_M2M]", claims["scopes"])
	}
	for _, absent := range []string{"user_roles", "roles", "cgen"} {
		if _, has := claims[absent]; has {
			t.Errorf("%s present on the OBO token: %v", absent, claims[absent])
		}
	}
	exp, _ := claims["exp"].(float64)
	if want := exp - float64(time.Now().Unix()); math.Abs(tok.ExpiresIn-want) > 1 {
		t.Errorf("expires_in = %v, want exp - now = %v", tok.ExpiresIn, want)
	}

	use := unauthRequest(t, http.MethodGet, "/api/model/", "Bearer "+tok.AccessToken)
	defer use.Body.Close()
	if use.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(use.Body)
		t.Fatalf("the exchanged token on GET /api/model/: %d; body: %s", use.StatusCode, body)
	}
}

// TestToken_ClientCredentials_OBOClient_400: an on-behalf-of client may only
// exchange user assertions.
func TestToken_ClientCredentials_OBOClient_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	id, secret := createClient(t, false, true)
	assertOAuthErrorDesc(t, postToken(t, suiteTenant, url.Values{"grant_type": {"client_credentials"}}, id, secret),
		http.StatusBadRequest, "unauthorized_client", descOnlyExchange)
}

// TestToken_Method_405: any method but POST is the endpoint's own
// OAuth-shaped 405. It runs on a stack of its own: the shared server's
// conformance validator records a request to an undeclared method as drift.
func TestToken_Method_405(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newCalloutHarnessWithKey(t, genKey(t), nil)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			req, err := http.NewRequestWithContext(e2eCtx(t), m, h.baseURL+"/api/tenants/"+suiteTenant+"/oauth/token", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want POST", got)
			}
			assertOAuthError(t, resp, http.StatusMethodNotAllowed, "method_not_allowed")
		})
	}
}

// TestToken_ContentTypeMustBeForm: the body must be
// application/x-www-form-urlencoded (a charset parameter is fine); a JSON
// body or none declared is 400 invalid_request, before the client
// authenticates.
func TestToken_ContentTypeMustBeForm(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	id, secret := createClient(t, false, false)
	post := func(t *testing.T, contentType, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(e2eCtx(t), http.MethodPost, serverURL+"/api/tenants/"+suiteTenant+"/oauth/token", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.SetBasicAuth(id, secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	const desc = "the request body must be application/x-www-form-urlencoded"
	t.Run("json", func(t *testing.T) {
		assertOAuthErrorDesc(t, post(t, "application/json", `{"grant_type":"client_credentials"}`),
			http.StatusBadRequest, "invalid_request", desc)
	})
	t.Run("missing", func(t *testing.T) {
		assertOAuthErrorDesc(t, post(t, "", "grant_type=client_credentials"), http.StatusBadRequest, "invalid_request", desc)
	})
	t.Run("charset accepted", func(t *testing.T) {
		resp := post(t, "application/x-www-form-urlencoded; charset=utf-8", "grant_type=client_credentials")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
	})
}

// TestToken_TokenExchange_InvalidatedKeyEndsAtOnce: trusted keys have no
// grace period. An exchange succeeds, the key is invalidated (no body), and
// the very next exchange is refused. A stray gracePeriodSec in the
// invalidate body changes nothing: the request has no body to read.
func TestToken_TokenExchange_InvalidatedKeyEndsAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	oboID, oboSecret := createClient(t, false, true)
	for name, body := range map[string][]byte{
		"no-body":          nil,
		"stray-grace-body": mustJSON(t, map[string]any{"gracePeriodSec": 3600}),
	} {
		t.Run(name, func(t *testing.T) {
			priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", nil)
			ok := exchangeAs(t, oboID, oboSecret, assertionFor(priv, kid, "alice", "test-tenant", nil))
			if ok.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(ok.Body)
				ok.Body.Close()
				t.Fatalf("exchange before invalidate: status=%d, want 200; body: %s", ok.StatusCode, raw)
			}
			ok.Body.Close()

			inv := adminRequest(t, "POST", "/oauth/keys/trusted/"+kid+"/invalidate", body)
			if inv.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(inv.Body)
				inv.Body.Close()
				t.Fatalf("invalidate %s: status=%d; body: %s", kid, inv.StatusCode, raw)
			}
			inv.Body.Close()

			assertOAuthErrorDesc(t, exchangeAs(t, oboID, oboSecret, assertionFor(priv, kid, "alice", "test-tenant", nil)),
				http.StatusBadRequest, "invalid_request", descKey)
		})
	}
}

// TestToken_TokenExchange_KeyNotYetValid_400: a key whose window opens later
// is refused like an unknown one.
func TestToken_TokenExchange_KeyNotYetValid_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	oboID, oboSecret := createClient(t, false, true)
	priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", map[string]any{
		"validFrom": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	assertOAuthErrorDesc(t, exchangeAs(t, oboID, oboSecret, assertionFor(priv, kid, "alice", "test-tenant", nil)),
		http.StatusBadRequest, "invalid_request", descKey)
}

// TestToken_TokenExchange_IssuerNotListed_400: a key that lists issuers
// accepts an assertion only from one of them.
func TestToken_TokenExchange_IssuerNotListed_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	oboID, oboSecret := createClient(t, false, true)
	priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", map[string]any{"issuers": []string{"e2e-app"}})
	listed := exchangeAs(t, oboID, oboSecret, assertionFor(priv, kid, "alice", "test-tenant", nil))
	listed.Body.Close()
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("listed issuer: %d, want 200", listed.StatusCode)
	}
	assertOAuthErrorDesc(t, exchangeAs(t, oboID, oboSecret, assertionFor(priv, kid, "alice", "test-tenant",
		func(c map[string]any) { c["iss"] = "elsewhere" })), http.StatusBadRequest, "invalid_request", descSig)
}

// TestToken_TokenExchange_KeyFromAnotherTenant_400: a trusted key belongs to
// the tenant that registered it. An OBO client of another tenant cannot
// exchange an assertion signed with it, even one whose caas_org_id names the
// client's own tenant and asks for ROLE_ADMIN.
func TestToken_TokenExchange_KeyFromAnotherTenant_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", nil) // registered in test-tenant
	otherTenant := fmt.Sprintf("e2e-tx-other-%d", time.Now().UnixNano())
	clientID, secret := createOBOClientIn(t, otherTenant)
	subject := assertionFor(priv, kid, "alice", otherTenant, func(c map[string]any) { c["user_roles"] = []string{"ROLE_ADMIN"} })
	assertOAuthErrorDesc(t, exchangeAsIn(t, otherTenant, clientID, secret, subject), http.StatusBadRequest, "invalid_request", descKey)
}

// TestToken_TokenExchange_SameKidTwoTenants_400: key ids are unique per
// tenant only. Tenants A and B hold different keys under one kid; A's OBO
// client presenting an assertion signed with B's key under that kid is
// refused — the key is read in A's tenant, and B's signature does not verify
// against it. Each tenant's own key still works for its own client.
func TestToken_TokenExchange_SameKidTwoTenants_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	tenantB := fmt.Sprintf("e2e-kid-b-%d", time.Now().UnixNano())
	kid := "same-kid-" + uuid.NewString()[:8]
	privA, _ := registerTrustedSignerAs(t, suiteToken(t), kid, nil)
	privB, _ := registerTrustedSignerAs(t, adminTokenForTenant(t, tenantB, "admin-b"), kid, nil)
	idA, secretA := createClient(t, false, true)
	idB, secretB := createOBOClientIn(t, tenantB)

	assertOAuthErrorDesc(t, exchangeAs(t, idA, secretA, assertionFor(privB, kid, "alice", "test-tenant", nil)),
		http.StatusBadRequest, "invalid_request", descSig)

	for name, c := range map[string]struct {
		id, secret, tenant string
		key                *rsa.PrivateKey
	}{"a": {idA, secretA, "test-tenant", privA}, "b": {idB, secretB, tenantB, privB}} {
		resp := exchangeAsIn(t, c.tenant, c.id, c.secret, assertionFor(c.key, kid, "alice", c.tenant, nil))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("tenant %s with its own key: %d, want 200", name, resp.StatusCode)
		}
	}
}

// TestToken_TokenExchange_OneClientManyUsers: one on-behalf-of client serves
// every user of its application — two exchanges through the same client and
// key carry different users and the same act.sub.
func TestToken_TokenExchange_OneClientManyUsers(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	alice := decodeJWTPayload(t, oboToken(t, "alice"))
	bob := decodeJWTPayload(t, oboTokenFor(t, suiteToken(t), "bob"))
	if alice["sub"] != "alice" || bob["sub"] != "bob" {
		t.Fatalf("sub alice=%v bob=%v", alice["sub"], bob["sub"])
	}
	actA, _ := alice["act"].(map[string]any)
	actB, _ := bob["act"].(map[string]any)
	if actA["sub"] == nil || actA["sub"] != actB["sub"] {
		t.Fatalf("act.sub alice=%v bob=%v, want the same client", actA["sub"], actB["sub"])
	}
}

// createOBOClientIn creates an OBO client in tenant through POST
// /clients?onBehalfOf=true, as a seed admin of that tenant. The client is
// deleted when the test ends.
func createOBOClientIn(t *testing.T, tenant string) (string, string) {
	t.Helper()
	seed := func() string { return adminTokenForTenant(t, tenant, "obo-seed") }
	resp := unauthRequest(t, http.MethodPost, "/api/clients?onBehalfOf=true", "Bearer "+seed())
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create OBO client in %s: %d %s", tenant, resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	cred := decodeCredential(t, "create OBO client", raw)
	deleteClientAtCleanup(t, serverURL, cred.id, seed)
	return cred.id, cred.secret
}
