package auth_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

const (
	// testIssuer is the issuer setupTokenEnv passes to NewTokenHandler; an
	// assertion's aud must name it.
	testIssuer = "cyoda"
	// testExpiry is the configured token lifetime of setupTokenEnv's handler.
	testExpiry = 300

	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	jwtTokenType       = "urn:ietf:params:oauth:token-type:jwt"
)

// testTokenEnv holds shared test fixtures for token endpoint tests.
type testTokenEnv struct {
	keyStore        *auth.KVKeyStore
	trustedKeyStore *auth.KVTrustedKeyStore
	m2mStore        *auth.KVM2MClientStore
	handler         http.Handler
	clientID        string // a plain client: client_credentials only
	clientSecret    string
	oboID           string // an on-behalf-of client: token exchange only
	oboSecret       string
	signingKey      *rsa.PrivateKey
	trustedKey      *rsa.PrivateKey
	trustedKID      string
	tenantID        string
}

func setupTokenEnv(t *testing.T) *testTokenEnv {
	t.Helper()

	// The token endpoint signs with the bootstrap key.
	signingKey := newBootstrap(t)
	keyStore := newTestKeyStore(t, signingKey)
	trustedKeyStore := newTestTrustedStore(t)
	m2mStore := auth.NewKVM2MClientStore(mustNewMemoryKV(t, systemCtx()), 0)

	// The application's assertion-signing key, registered in the tenant.
	trustedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate trusted key: %v", err)
	}
	trustedKID := "trusted-kid-1"
	tenantID := "tenant-abc"
	err = trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID:       trustedKID,
		TenantID:  spi.TenantID(tenantID),
		PublicKey: &trustedKey.PublicKey,
		Active:    true,
		ValidFrom: time.Now().Add(-time.Hour),
	}, false)
	if err != nil {
		t.Fatalf("failed to register trusted key: %v", err)
	}
	// A client's user id is its client id.
	clientID := "TESTM2MCLIENT"
	clientSecret, err := m2mStore.Create(systemCtx(), spi.TenantID(tenantID), clientID, clientID, []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatalf("failed to create M2M client: %v", err)
	}
	oboID := "TESTOBOCLIENT"
	oboSecret, err := m2mStore.Create(systemCtx(), spi.TenantID(tenantID), oboID, oboID, []string{"ROLE_M2M"}, true)
	if err != nil {
		t.Fatalf("failed to create OBO client: %v", err)
	}

	return &testTokenEnv{
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		m2mStore:        m2mStore,
		handler:         auth.NewTokenHandler(keyStore, trustedKeyStore, m2mStore, testIssuer, "", testExpiry),
		clientID:        clientID,
		clientSecret:    clientSecret,
		oboID:           oboID,
		oboSecret:       oboSecret,
		signingKey:      signingKey,
		trustedKey:      trustedKey,
		trustedKID:      trustedKID,
		tenantID:        tenantID,
	}
}

// withHandler replaces env's handler with one built from env's stores and
// the given audience and expiry.
func (e *testTokenEnv) withHandler(audience string, expiry int) *testTokenEnv {
	e.handler = auth.NewTokenHandler(e.keyStore, e.trustedKeyStore, e.m2mStore, testIssuer, audience, expiry)
	return e
}

// assertion is a valid user assertion for alice in env's tenant, signed with
// env's trusted key; mutate edits the claims before signing.
func (e *testTokenEnv) assertion(t *testing.T, mutate func(c map[string]any)) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{"sub": "alice", "iss": "app", "aud": []string{testIssuer},
		"caas_org_id": e.tenantID, "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix()}
	if mutate != nil {
		mutate(c)
	}
	return signSubjectToken(t, e.trustedKey, e.trustedKID, c)
}

// exchange runs the token-exchange grant as client id with form, defaulting
// subject_token_type to the JWT type.
func (e *testTokenEnv) exchange(t *testing.T, id, secret string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if _, set := form["subject_token_type"]; !set {
		form.Set("subject_token_type", jwtTokenType)
	}
	req := makeTokenRequest(tokenExchangeGrant, basicAuth(id, secret), form)
	rr := httptest.NewRecorder()
	e.handler.ServeHTTP(rr, req)
	return rr
}

// clientCredentials runs the client_credentials grant as client id.
func (e *testTokenEnv) clientCredentials(t *testing.T, id, secret string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	e.handler.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(id, secret), nil))
	return rr
}

// issued decodes a 200 token response and returns the body and the issued
// token's claims; any other status fails the test.
func issued(t *testing.T, rr *httptest.ResponseRecorder) (map[string]any, map[string]any) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeResponse(t, rr)
	tok, _ := body["access_token"].(string)
	p, err := auth.Parse(tok)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	return body, p.Claims
}

func basicAuth(clientID, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(clientID+":"+secret))
}

func makeTokenRequest(grantType, authHeader string, extraForm url.Values) *http.Request {
	form := url.Values{}
	form.Set("grant_type", grantType)
	for k, vs := range extraForm {
		for _, v := range vs {
			form.Set(k, v)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func decodeResponse(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

// signSubjectToken signs claims as an RS256 JWT with key under kid, as an
// application signs a user assertion.
func signSubjectToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(key), kid)
	if err != nil {
		t.Fatalf("failed to sign subject token: %v", err)
	}
	return token
}

// signWithHeader signs claims with key (RS256 signature bytes) under an
// arbitrary header, so a test can present a token whose header lies about
// its algorithm.
func signWithHeader(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// numClaim returns a numeric claim as an int64.
func numClaim(t *testing.T, claims map[string]any, name string) int64 {
	t.Helper()
	f, ok := claims[name].(float64)
	if !ok {
		t.Fatalf("claim %s = %v, want a number", name, claims[name])
	}
	return int64(f)
}

func TestTokenEndpoint_ClientCredentialsCarriesConfiguredAudience(t *testing.T) {
	env := setupTokenEnv(t).withHandler("cyoda-api", testExpiry)
	_, claims := issued(t, env.clientCredentials(t, env.clientID, env.clientSecret))
	if claims["aud"] != "cyoda-api" {
		t.Fatalf("aud = %v, want cyoda-api", claims["aud"])
	}
}

func TestTokenEndpoint_NoAudienceConfiguredOmitsAud(t *testing.T) {
	env := setupTokenEnv(t)
	_, claims := issued(t, env.clientCredentials(t, env.clientID, env.clientSecret))
	if _, present := claims["aud"]; present {
		t.Fatalf("aud present without a configured audience: %v", claims["aud"])
	}
}

// failingM2MStore fails Authenticate with err.
type failingM2MStore struct {
	auth.M2MClientStore
	err error
}

func (f failingM2MStore) Authenticate(context.Context, string, string) (*auth.M2MClient, error) {
	return nil, f.err
}

// A client store that cannot answer is the server failing, never the
// client's credentials being wrong: storage unavailable is 503
// temporarily_unavailable with Retry-After: 1, any other failure 500
// server_error with a ticket — never 401 invalid_client. Neither response
// carries the cause.
func TestToken_ClientStoreUnavailable_503(t *testing.T) {
	env := setupTokenEnv(t)
	for name, c := range map[string]struct {
		store      auth.M2MClientStore
		status     int
		code       string
		retryAfter string
	}{
		"unavailable": {auth.NewKVM2MClientStore(brokenKV{}, 0), http.StatusServiceUnavailable, "temporarily_unavailable", "1"},
		"other":       {failingM2MStore{err: errors.New("disk on fire")}, http.StatusInternalServerError, "server_error", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, c.store, testIssuer, "", testExpiry)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
			if rr.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", rr.Code, c.status, rr.Body.String())
			}
			if got := rr.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
			if strings.Contains(rr.Body.String(), "storage down") || strings.Contains(rr.Body.String(), "disk on fire") {
				t.Errorf("response leaks the cause: %s", rr.Body.String())
			}
			resp := decodeResponse(t, rr)
			if resp["error"] != c.code {
				t.Errorf("error = %v, want %s", resp["error"], c.code)
			}
			if c.status == http.StatusInternalServerError && !strings.HasPrefix(fmt.Sprint(resp["error_description"]), "server_error [ticket: ") {
				t.Errorf("error_description = %v, want a ticket", resp["error_description"])
			}
		})
	}
}

// An id outside the client-id grammar is 401 invalid_client and never
// reaches the store: the store here fails every call, so a read would
// answer 503.
func TestTokenEndpoint_MalformedClientIDIs401(t *testing.T) {
	env := setupTokenEnv(t)
	h := auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, auth.NewKVM2MClientStore(brokenKV{}, 0), testIssuer, "", testExpiry)
	for _, raw := range []string{"a%00b", "%FF", strings.Repeat("A", 101), "a%3Ab"} {
		req := makeTokenRequest("client_credentials", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw+":x")), nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || decodeResponse(t, rr)["error"] != "invalid_client" {
			t.Fatalf("%q: %d %s", raw, rr.Code, rr.Body.String())
		}
	}
}

// §4.2: a client-credentials token carries the client as sub and
// caas_user_id, its tenant, its roles, its secret generation, and an
// expires_in equal to the token's remaining life.
func TestTokenClientCredentialsValid(t *testing.T) {
	env := setupTokenEnv(t)
	rr := env.clientCredentials(t, env.clientID, env.clientSecret)
	body, claims := issued(t, rr)
	if body["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", body["token_type"])
	}
	tok, _ := body["access_token"].(string)
	parsed, err := auth.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &env.signingKey.PublicKey); err != nil {
		t.Fatalf("token signature verification failed: %v", err)
	}
	for k, want := range map[string]any{
		"sub": env.clientID, "caas_user_id": env.clientID, "caas_org_id": env.tenantID,
		"iss": testIssuer, "caas_tier": "unlimited", "cgen": float64(1),
	} {
		if claims[k] != want {
			t.Errorf("%s = %v, want %v", k, claims[k], want)
		}
	}
	if scopes, _ := claims["scopes"].([]any); len(scopes) != 1 || scopes[0] != "ROLE_M2M" {
		t.Errorf("scopes = %v, want [ROLE_M2M]", claims["scopes"])
	}
	if _, err := uuid.Parse(fmt.Sprint(claims["jti"])); err != nil {
		t.Errorf("jti = %v, want a uuid", claims["jti"])
	}
	for _, absent := range []string{"act", "user_roles"} {
		if _, has := claims[absent]; has {
			t.Errorf("%s present on a client-credentials token: %v", absent, claims[absent])
		}
	}
	exp, iat := numClaim(t, claims, "exp"), numClaim(t, claims, "iat")
	if exp-iat != testExpiry {
		t.Errorf("exp - iat = %d, want %d", exp-iat, testExpiry)
	}
	if got, want := body["expires_in"].(float64), float64(exp-time.Now().Unix()); math.Abs(got-want) > 1 {
		t.Errorf("expires_in = %v, want exp - now = %v", got, want)
	}
}

func TestTokenClientCredentialsInvalidSecret(t *testing.T) {
	env := setupTokenEnv(t)
	rr := env.clientCredentials(t, env.clientID, "wrong-secret")
	if rr.Code != http.StatusUnauthorized || decodeResponse(t, rr)["error"] != "invalid_client" {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTokenClientCredentialsUnknownClient(t *testing.T) {
	env := setupTokenEnv(t)
	rr := env.clientCredentials(t, "unknownclient", "some-secret")
	if rr.Code != http.StatusUnauthorized || decodeResponse(t, rr)["error"] != "invalid_client" {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

// Every 401 carries WWW-Authenticate: Basic and the fixed description,
// whatever made the client authentication fail.
func TestToken_401CarriesWWWAuthenticate(t *testing.T) {
	env := setupTokenEnv(t)
	for name, header := range map[string]string{
		"wrong secret":   basicAuth(env.clientID, "wrong"),
		"unknown client": basicAuth("unknownclient", "x"),
		"no header":      "",
		"not basic":      "Bearer abc",
		"not base64":     "Basic !!!",
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			env.handler.ServeHTTP(rr, makeTokenRequest("client_credentials", header, nil))
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != "Basic" {
				t.Errorf("WWW-Authenticate = %q, want Basic", got)
			}
			resp := decodeResponse(t, rr)
			if resp["error"] != "invalid_client" || resp["error_description"] != "client authentication failed" {
				t.Errorf("body = %v", resp)
			}
		})
	}
}

// §3.1: an on-behalf-of client may only exchange user assertions.
func TestTokenClientCredentials_OBOClientRefused(t *testing.T) {
	env := setupTokenEnv(t)
	rr := env.clientCredentials(t, env.oboID, env.oboSecret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp["error"] != "unauthorized_client" || resp["error_description"] != "this client may only exchange user assertions" {
		t.Fatalf("body = %v", resp)
	}
}

// cgen is the client's secret generation: 1 at creation, 2 after one reset.
func TestTokenClientCredentials_CarriesCgen(t *testing.T) {
	env := setupTokenEnv(t)
	if _, claims := issued(t, env.clientCredentials(t, env.clientID, env.clientSecret)); claims["cgen"] != float64(1) {
		t.Fatalf("cgen = %v, want 1", claims["cgen"])
	}
	secret, _, err := env.m2mStore.ResetSecret(systemCtx(), spi.TenantID(env.tenantID), env.clientID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, claims := issued(t, env.clientCredentials(t, env.clientID, secret)); claims["cgen"] != float64(2) {
		t.Fatalf("cgen after reset = %v, want 2", claims["cgen"])
	}
}

// TestTokenExchange_Refusals covers every §12.1 refusal of the exchange that
// a request can trigger: status, error code and the fixed description, which
// never echoes the tenant or the key id.
func TestTokenExchange_Refusals(t *testing.T) {
	env := setupTokenEnv(t)
	type tc struct {
		name   string
		id     func() (string, string)
		form   func() url.Values
		status int
		code   string
		desc   string
	}
	obo := func() (string, string) { return env.oboID, env.oboSecret }
	plain := func() (string, string) { return env.clientID, env.clientSecret }
	with := func(a string, extra ...string) func() url.Values {
		return func() url.Values {
			f := url.Values{"subject_token": {a}}
			for i := 0; i+1 < len(extra); i += 2 {
				f.Set(extra[i], extra[i+1])
			}
			return f
		}
	}
	const (
		descNotOBO   = "this client may not exchange user assertions"
		descParam    = "unsupported parameter"
		descType     = "unsupported subject_token_type"
		descInvalid  = "invalid subject token"
		descKey      = "unknown or inactive trusted key"
		descSig      = "subject token signature or issuer rejected"
		descClaims   = "subject token claims rejected"
		descTenant   = "tenant mismatch"
		descSub      = "subject token sub rejected"
		descExpired  = "subject token has expired"
		invalidReq   = "invalid_request"
		unauthorized = "unauthorized_client"
	)
	ok := env.assertion(t, nil)
	now := time.Now()
	validClaims := map[string]any{"sub": "alice", "aud": testIssuer, "caas_org_id": env.tenantID,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	cases := []tc{
		{"non-OBO client", plain, with(ok), 400, unauthorized, descNotOBO},
		{"non-OBO client, garbage token", plain, with("not-a-jwt"), 400, unauthorized, descNotOBO},
		{"actor_token", obo, with(ok, "actor_token", "x"), 400, invalidReq, descParam},
		{"empty scope present", obo, with(ok, "scope", ""), 400, invalidReq, descParam},
		{"resource", obo, with(ok, "resource", "x"), 400, invalidReq, descParam},
		{"audience", obo, with(ok, "audience", "x"), 400, invalidReq, descParam},
		{"requested_token_type", obo, with(ok, "requested_token_type", "x"), 400, invalidReq, descParam},
		{"actor_token_type", obo, with(ok, "actor_token_type", "x"), 400, invalidReq, descParam},
		{"access_token type", obo, with(ok, "subject_token_type", "urn:ietf:params:oauth:token-type:access_token"), 400, invalidReq, descType},
		{"empty subject_token_type", obo, with(ok, "subject_token_type", ""), 400, invalidReq, descType},
		{"garbage", obo, with("not-a-jwt"), 400, invalidReq, descInvalid},
		{"no subject_token", obo, func() url.Values { return url.Values{} }, 400, invalidReq, descInvalid},
		{"alg HS256", obo, with(signWithHeader(t, env.trustedKey, map[string]any{"alg": "HS256", "kid": env.trustedKID}, validClaims)), 400, invalidReq, descInvalid},
		{"alg none", obo, with(signWithHeader(t, env.trustedKey, map[string]any{"alg": "none", "kid": env.trustedKID}, validClaims)), 400, invalidReq, descInvalid},
		{"no kid", obo, with(signWithHeader(t, env.trustedKey, map[string]any{"alg": "RS256"}, validClaims)), 400, invalidReq, descInvalid},
		{"empty kid", obo, with(signSubjectToken(t, env.trustedKey, "", validClaims)), 400, invalidReq, descInvalid},
		{"unknown kid", obo, with(signSubjectToken(t, env.trustedKey, "nope", map[string]any{"sub": "alice"})), 400, invalidReq, descKey},
		{"wrong key", obo, with(signSubjectToken(t, generateTestKey(t), env.trustedKID, map[string]any{"sub": "alice"})), 400, invalidReq, descSig},
		{"aud missing", obo, with(env.assertion(t, func(c map[string]any) { delete(c, "aud") })), 400, invalidReq, descClaims},
		{"aud other", obo, with(env.assertion(t, func(c map[string]any) { c["aud"] = "elsewhere" })), 400, invalidReq, descClaims},
		{"aud not a string", obo, with(env.assertion(t, func(c map[string]any) { c["aud"] = 7 })), 400, invalidReq, descClaims},
		{"no exp", obo, with(env.assertion(t, func(c map[string]any) { delete(c, "exp") })), 400, invalidReq, descClaims},
		{"exp not a number", obo, with(env.assertion(t, func(c map[string]any) { c["exp"] = "soon" })), 400, invalidReq, descClaims},
		{"no iat", obo, with(env.assertion(t, func(c map[string]any) { delete(c, "iat") })), 400, invalidReq, descClaims},
		{"too long", obo, with(env.assertion(t, func(c map[string]any) { c["exp"] = time.Now().Add(301 * time.Second).Unix() })), 400, invalidReq, descClaims},
		{"iat future", obo, with(env.assertion(t, func(c map[string]any) {
			c["iat"] = time.Now().Add(time.Minute).Unix()
			c["exp"] = time.Now().Add(2 * time.Minute).Unix()
		})), 400, invalidReq, descClaims},
		{"expired", obo, with(env.assertion(t, func(c map[string]any) {
			c["iat"] = time.Now().Add(-5 * time.Minute).Unix()
			c["exp"] = time.Now().Add(-time.Minute).Unix()
		})), 400, invalidReq, descClaims},
		{"nbf future", obo, with(env.assertion(t, func(c map[string]any) { c["nbf"] = time.Now().Add(time.Minute).Unix() })), 400, invalidReq, descClaims},
		{"nbf not a number", obo, with(env.assertion(t, func(c map[string]any) { c["nbf"] = "later" })), 400, invalidReq, descClaims},
		{"tenant mismatch", obo, with(env.assertion(t, func(c map[string]any) { c["caas_org_id"] = "other" })), 403, "access_denied", descTenant},
		{"tenant missing", obo, with(env.assertion(t, func(c map[string]any) { delete(c, "caas_org_id") })), 403, "access_denied", descTenant},
		{"sub system", obo, with(env.assertion(t, func(c map[string]any) { c["sub"] = "SYSTEM" })), 400, invalidReq, descSub},
		{"sub empty", obo, with(env.assertion(t, func(c map[string]any) { c["sub"] = "" })), 400, invalidReq, descSub},
		{"sub missing", obo, with(env.assertion(t, func(c map[string]any) { delete(c, "sub") })), 400, invalidReq, descSub},
		{"sub newline", obo, with(env.assertion(t, func(c map[string]any) { c["sub"] = "ext\nuser" })), 400, invalidReq, descSub},
		{"sub too long", obo, with(env.assertion(t, func(c map[string]any) { c["sub"] = strings.Repeat("u", 256) })), 400, invalidReq, descSub},
		// exp inside the 30 s skew passes the claim check, but the token it
		// would cap to is not in the future.
		{"capped exp not in the future", obo, with(env.assertion(t, func(c map[string]any) {
			c["iat"] = time.Now().Add(-time.Minute).Unix()
			c["exp"] = time.Now().Add(-5 * time.Second).Unix()
		})), 400, invalidReq, descExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, sec := c.id()
			rr := env.exchange(t, id, sec, c.form())
			body := decodeResponse(t, rr)
			if rr.Code != c.status || body["error"] != c.code {
				t.Fatalf("got %d %v, want %d %s", rr.Code, body, c.status, c.code)
			}
			if body["error_description"] != c.desc {
				t.Errorf("error_description = %v, want %q", body["error_description"], c.desc)
			}
			if strings.Contains(fmt.Sprint(body["error_description"]), env.tenantID) ||
				strings.Contains(fmt.Sprint(body["error_description"]), env.trustedKID) {
				t.Fatalf("description echoes tenant or kid: %v", body)
			}
		})
	}
}

// §4.4: the OBO token carries the asserted user, the client as act.sub, the
// client's roles only, the client's tenant, no user_roles and no cgen; the
// assertion's roles never reach it.
func TestTokenExchange_Claims(t *testing.T) {
	env := setupTokenEnv(t).withHandler("cyoda-api", testExpiry)
	a := env.assertion(t, func(c map[string]any) {
		c["user_roles"] = []string{"ROLE_ADMIN"}
		c["roles"] = []string{"ROLE_ADMIN"}
		c["scopes"] = []string{"ROLE_ADMIN"}
	})
	body, claims := issued(t, env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {a}}))
	if body["issued_token_type"] != jwtTokenType || body["token_type"] != "Bearer" {
		t.Errorf("body = %v", body)
	}
	tok, _ := body["access_token"].(string)
	parsed, _ := auth.Parse(tok)
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &env.signingKey.PublicKey); err != nil {
		t.Fatalf("OBO token signature: %v", err)
	}
	for k, want := range map[string]any{
		"sub": "alice", "caas_user_id": "alice", "caas_org_id": env.tenantID,
		"iss": testIssuer, "aud": "cyoda-api", "caas_tier": "unlimited",
	} {
		if claims[k] != want {
			t.Errorf("%s = %v, want %v", k, claims[k], want)
		}
	}
	act, _ := claims["act"].(map[string]any)
	if len(act) != 1 || act["sub"] != env.oboID {
		t.Errorf("act = %v, want exactly {sub: %s}", claims["act"], env.oboID)
	}
	if scopes, _ := claims["scopes"].([]any); len(scopes) != 1 || scopes[0] != "ROLE_M2M" {
		t.Errorf("scopes = %v, want the client's [ROLE_M2M]", claims["scopes"])
	}
	for _, absent := range []string{"user_roles", "roles", "cgen"} {
		if _, has := claims[absent]; has {
			t.Errorf("%s present on an OBO token: %v", absent, claims[absent])
		}
	}
	if _, err := uuid.Parse(fmt.Sprint(claims["jti"])); err != nil {
		t.Errorf("jti = %v, want a uuid", claims["jti"])
	}
	exp := numClaim(t, claims, "exp")
	if got, want := body["expires_in"].(float64), float64(exp-time.Now().Unix()); math.Abs(got-want) > 1 {
		t.Errorf("expires_in = %v, want exp - now = %v", got, want)
	}
}

// exp = min(assertion exp, now + expiry): an assertion ending in 60 s caps
// the token at its own exp.
func TestTokenExchange_ExpCappedByAssertion(t *testing.T) {
	env := setupTokenEnv(t)
	aexp := time.Now().Add(60 * time.Second).Unix()
	a := env.assertion(t, func(c map[string]any) { c["exp"] = aexp })
	body, claims := issued(t, env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {a}}))
	if got := numClaim(t, claims, "exp"); got != aexp {
		t.Errorf("exp = %d, want the assertion's %d", got, aexp)
	}
	if got := body["expires_in"].(float64); math.Abs(got-60) > 1 {
		t.Errorf("expires_in = %v, want about 60", got)
	}
}

// A configured expiry shorter than the assertion's remaining life caps the
// token at now + expiry.
func TestTokenExchange_ExpCappedByConfig(t *testing.T) {
	env := setupTokenEnv(t).withHandler("", 30)
	body, claims := issued(t, env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, nil)}}))
	if d := numClaim(t, claims, "exp") - numClaim(t, claims, "iat"); d != 30 {
		t.Errorf("exp - iat = %d, want 30", d)
	}
	if got := body["expires_in"].(float64); math.Abs(got-30) > 1 {
		t.Errorf("expires_in = %v, want about 30", got)
	}
}

// A key that lists issuers accepts an assertion only from one of them.
func TestTokenExchange_IssuerAllowList(t *testing.T) {
	env := setupTokenEnv(t)
	if err := env.trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID: env.trustedKID, TenantID: spi.TenantID(env.tenantID), PublicKey: &env.trustedKey.PublicKey,
		Issuers: []string{"app"}, Active: true, ValidFrom: time.Now().Add(-time.Hour),
	}, false); err != nil {
		t.Fatalf("re-register with issuers: %v", err)
	}
	if rr := env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, nil)}}); rr.Code != http.StatusOK {
		t.Fatalf("listed issuer: %d %s", rr.Code, rr.Body.String())
	}
	for name, mutate := range map[string]func(map[string]any){
		"other issuer": func(c map[string]any) { c["iss"] = "elsewhere" },
		"no issuer":    func(c map[string]any) { delete(c, "iss") },
	} {
		t.Run(name, func(t *testing.T) {
			rr := env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, mutate)}})
			resp := decodeResponse(t, rr)
			if rr.Code != http.StatusBadRequest || resp["error"] != "invalid_request" ||
				resp["error_description"] != "subject token signature or issuer rejected" {
				t.Fatalf("got %d %v", rr.Code, resp)
			}
		})
	}
}

// assertRefusedKey asserts rr is the refusal of a key that is not usable in
// the client's tenant.
func assertRefusedKey(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	resp := decodeResponse(t, rr)
	if rr.Code != http.StatusBadRequest || resp["error"] != "invalid_request" || resp["error_description"] != "unknown or inactive trusted key" {
		t.Fatalf("got %d %v, want 400 invalid_request / unknown or inactive trusted key", rr.Code, resp)
	}
}

// A trusted key belongs to the tenant that registered it. An assertion
// signed with another tenant's key is refused, even when its caas_org_id
// names the exchanging client's tenant — the claim is written by the key
// holder, so it cannot be what binds the key to a tenant.
func TestTokenExchange_KeyFromAnotherTenant(t *testing.T) {
	env := setupTokenEnv(t)
	otherKey := generateTestKey(t)
	const otherKID = "other-tenant-kid"
	if err := env.trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID: otherKID, TenantID: spi.TenantID("tenant-other"), PublicKey: &otherKey.PublicKey,
		Active: true, ValidFrom: time.Now().Add(-time.Hour),
	}, false); err != nil {
		t.Fatalf("register other tenant's key: %v", err)
	}
	now := time.Now()
	a := signSubjectToken(t, otherKey, otherKID, map[string]any{
		"sub": "alice", "aud": testIssuer, "caas_org_id": env.tenantID,
		"user_roles": []string{"ROLE_ADMIN"}, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	})
	assertRefusedKey(t, env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {a}}))
}

// Key ids are unique per tenant only. When two tenants hold the same kid
// with different keys, the key is read in the client's tenant: an assertion
// signed with the other tenant's key under that kid fails the signature
// check against the client's own key.
func TestTokenExchange_SameKidOtherTenantsKey(t *testing.T) {
	env := setupTokenEnv(t)
	otherKey := generateTestKey(t)
	if err := env.trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID: env.trustedKID, TenantID: spi.TenantID("tenant-other"), PublicKey: &otherKey.PublicKey,
		Active: true, ValidFrom: time.Now().Add(-time.Hour),
	}, false); err != nil {
		t.Fatalf("register the same kid in another tenant: %v", err)
	}
	now := time.Now()
	a := signSubjectToken(t, otherKey, env.trustedKID, map[string]any{
		"sub": "alice", "aud": testIssuer, "caas_org_id": env.tenantID,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	})
	rr := env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {a}})
	resp := decodeResponse(t, rr)
	if rr.Code != http.StatusBadRequest || resp["error"] != "invalid_request" || resp["error_description"] != "subject token signature or issuer rejected" {
		t.Fatalf("got %d %v", rr.Code, resp)
	}
}

// A key whose window has not opened yet is refused like an unknown one: the
// store decides, on every exchange.
func TestTokenExchange_KeyNotYetValid(t *testing.T) {
	env := setupTokenEnv(t)
	if err := env.trustedKeyStore.Register(context.Background(), &auth.TrustedKey{
		KID: env.trustedKID, TenantID: spi.TenantID(env.tenantID), PublicKey: &env.trustedKey.PublicKey,
		Active: true, ValidFrom: time.Now().Add(time.Hour),
	}, false); err != nil {
		t.Fatalf("re-register ahead: %v", err)
	}
	assertRefusedKey(t, env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, nil)}}))
}

// An invalidated key ends at once: the next exchange is refused.
func TestTokenExchange_InactiveTrustedKey(t *testing.T) {
	env := setupTokenEnv(t)
	if rr := env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, nil)}}); rr.Code != http.StatusOK {
		t.Fatalf("before invalidate: %d %s", rr.Code, rr.Body.String())
	}
	if err := env.trustedKeyStore.Invalidate(context.Background(), spi.TenantID(env.tenantID), env.trustedKID); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assertRefusedKey(t, env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, nil)}}))
}

// The exchanged token carries the assertion's sub as its user id, so a sub
// the server would reject on every later request is rejected here; the
// response never repeats the value.
func TestTokenExchange_InvalidSubClaim(t *testing.T) {
	env := setupTokenEnv(t)
	for name, sub := range map[string]string{
		"newline":  "ext\nuser",
		"nul":      "ext\x00user",
		"too-long": strings.Repeat("u", 256),
		"reserved": "SYSTEM",
	} {
		t.Run(name, func(t *testing.T) {
			rr := env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, func(c map[string]any) { c["sub"] = sub })}})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), sub) {
				t.Errorf("response echoes the rejected sub: %s", rr.Body.String())
			}
			if resp := decodeResponse(t, rr); resp["error"] != "invalid_request" {
				t.Errorf("error = %v, want invalid_request", resp["error"])
			}
		})
	}
}

func TestTokenUnsupportedGrantType(t *testing.T) {
	env := setupTokenEnv(t)
	for _, id := range []struct{ id, secret string }{{env.clientID, env.clientSecret}, {env.oboID, env.oboSecret}} {
		rr := httptest.NewRecorder()
		env.handler.ServeHTTP(rr, makeTokenRequest("authorization_code", basicAuth(id.id, id.secret), nil))
		if rr.Code != http.StatusBadRequest || decodeResponse(t, rr)["error"] != "unsupported_grant_type" {
			t.Fatalf("%s: status %d: %s", id.id, rr.Code, rr.Body.String())
		}
	}
}

// A body that does not parse as a form, or exceeds the 1 MiB cap, is
// 400 invalid_request once the client has authenticated.
func TestToken_MalformedBody_400(t *testing.T) {
	env := setupTokenEnv(t)
	for name, body := range map[string]string{
		"bad escape": "grant_type=client_credentials&x=%zz",
		"oversized":  "grant_type=client_credentials&x=" + strings.Repeat("a", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", basicAuth(env.clientID, env.clientSecret))
			rr := httptest.NewRecorder()
			env.handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest || decodeResponse(t, rr)["error"] != "invalid_request" {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestTokenHandler_NonPost_405MethodNotAllowed(t *testing.T) {
	env := setupTokenEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/oauth/token", nil)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || decodeResponse(t, rr)["error"] != "method_not_allowed" {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

// failingTrustedKeyStore fails GetForVerification with err.
type failingTrustedKeyStore struct {
	auth.TrustedKeyStore
	err error
}

func (f failingTrustedKeyStore) GetForVerification(context.Context, spi.TenantID, string) (*auth.TrustedKey, error) {
	return nil, f.err
}

// A trusted-key store that cannot answer fails the exchange — it is never
// read as "no such key". Storage unavailable: 503 temporarily_unavailable
// with Retry-After: 1. Any other store failure: 500 server_error with a
// ticket. Neither response carries the cause.
func TestTokenExchange_TrustedKeyStoreUnavailable_503(t *testing.T) {
	env := setupTokenEnv(t)
	for name, c := range map[string]struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		"unavailable": {fmt.Errorf("failed to read trusted key: %w", unavailable{}), http.StatusServiceUnavailable, "temporarily_unavailable", "1"},
		"other":       {errors.New("failed to read trusted key: disk on fire"), http.StatusInternalServerError, "server_error", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := auth.NewTokenHandler(env.keyStore, failingTrustedKeyStore{err: c.err}, env.m2mStore, testIssuer, "", testExpiry)
			rr := httptest.NewRecorder()
			form := url.Values{"subject_token": {env.assertion(t, nil)}, "subject_token_type": {jwtTokenType}}
			h.ServeHTTP(rr, makeTokenRequest(tokenExchangeGrant, basicAuth(env.oboID, env.oboSecret), form))
			if rr.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", rr.Code, c.status, rr.Body.String())
			}
			if got := rr.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
			if strings.Contains(rr.Body.String(), "storage down") || strings.Contains(rr.Body.String(), "disk on fire") {
				t.Errorf("response leaks the cause: %s", rr.Body.String())
			}
			resp := decodeResponse(t, rr)
			if resp["error"] != c.code {
				t.Errorf("error = %v, want %s", resp["error"], c.code)
			}
			if c.status == http.StatusInternalServerError && !strings.HasPrefix(fmt.Sprint(resp["error_description"]), "server_error [ticket: ") {
				t.Errorf("error_description = %v, want a ticket", resp["error_description"])
			}
		})
	}
}

// failingKeyStore is an auth.KeyStore whose Signer always fails; the other
// methods are not exercised by this test and return the same error.
type failingKeyStore struct{ err error }

func (f failingKeyStore) Signer() (*auth.KeyPair, auth.Signer, error)    { return nil, nil, f.err }
func (f failingKeyStore) Current() (*auth.KeyPair, error)                { return nil, f.err }
func (f failingKeyStore) VerificationKey(string) (*rsa.PublicKey, error) { return nil, f.err }
func (f failingKeyStore) Published() ([]*auth.KeyPair, error)            { return nil, f.err }
func (f failingKeyStore) Issue(context.Context, auth.IssueRequest) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Invalidate(context.Context, string, int64) error { return f.err }
func (f failingKeyStore) Reactivate(context.Context, string, time.Time, time.Time) (*auth.KeyPair, error) {
	return nil, f.err
}
func (f failingKeyStore) Delete(context.Context, string) error { return f.err }

// TestTokenEndpoint_ServerErrorCarriesTicket pins Gate 3 for this endpoint:
// every 5xx carries a generic message plus a ticket UUID and no internals.
// The OAuth2 body shape has no dedicated field, so the ticket rides in
// error_description, which the schema already declares as a string.
//
// A ticket is only worth minting if an operator can find it: the same UUID
// must appear in the log record that carries the underlying cause, which is
// what ties a caller's report to the failure. The log is captured here rather
// than asserted through a running stack, because inducing a key-store failure
// over HTTP would need a production seam this project forbids. Both grants
// mint through the same signing call.
func TestTokenEndpoint_ServerErrorCarriesTicket(t *testing.T) {
	env := setupTokenEnv(t)
	cause := errors.New("hsm unreachable at 10.0.0.5:8443")
	h := auth.NewTokenHandler(failingKeyStore{err: cause}, env.trustedKeyStore, env.m2mStore, testIssuer, "", testExpiry)

	for name, req := range map[string]func() *http.Request{
		"client_credentials": func() *http.Request {
			return makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil)
		},
		"token_exchange": func() *http.Request {
			return makeTokenRequest(tokenExchangeGrant, basicAuth(env.oboID, env.oboSecret),
				url.Values{"subject_token": {env.assertion(t, nil)}, "subject_token_type": {jwtTokenType}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logBuf bytes.Buffer
			prevLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			t.Cleanup(func() { slog.SetDefault(prevLogger) })

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req())
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", rr.Code, rr.Body.String())
			}
			resp := decodeResponse(t, rr)
			if resp["error"] != "server_error" {
				t.Errorf("error = %v, want server_error", resp["error"])
			}
			desc, _ := resp["error_description"].(string)
			if !strings.HasPrefix(desc, "server_error [ticket: ") {
				t.Fatalf("error_description = %q, want a ticket", desc)
			}
			ticket := strings.TrimSuffix(strings.TrimPrefix(desc, "server_error [ticket: "), "]")
			if _, err := uuid.Parse(ticket); err != nil {
				t.Errorf("ticket %q is not a uuid: %v", ticket, err)
			}
			if strings.Contains(rr.Body.String(), "hsm") || strings.Contains(rr.Body.String(), "10.0.0.5") {
				t.Errorf("response leaks the underlying cause: %s", rr.Body.String())
			}
			// The same ticket reaches the log, with the cause the response withheld.
			logged := logBuf.String()
			if !strings.Contains(logged, ticket) {
				t.Errorf("ticket %q never reaches the log:\n%s", ticket, logged)
			}
			if !strings.Contains(logged, "hsm unreachable") {
				t.Errorf("the underlying cause is missing from the log record:\n%s", logged)
			}
		})
	}
}

// An error response from the token endpoint is not cacheable either, so
// every /oauth/token response carries the same headers.
func TestTokenEndpoint_ErrorResponsesAreNotCacheable(t *testing.T) {
	env := setupTokenEnv(t)
	rr := env.clientCredentials(t, env.clientID, "wrong")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
}

// RFC 6749 §5.1: a response carrying a token has Cache-Control: no-store and
// Pragma: no-cache, on both grants.
func TestTokenEndpoint_TokenResponsesAreNotCacheable(t *testing.T) {
	env := setupTokenEnv(t)
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"client_credentials": env.clientCredentials(t, env.clientID, env.clientSecret),
		"token_exchange":     env.exchange(t, env.oboID, env.oboSecret, url.Values{"subject_token": {env.assertion(t, nil)}}),
	} {
		t.Run(name, func(t *testing.T) {
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := rr.Header().Get("Pragma"); got != "no-cache" {
				t.Errorf("Pragma = %q, want no-cache", got)
			}
		})
	}
}
