package parity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// OBOClient is an on-behalf-of client of a tenant and the trusted key its
// application signs user assertions with.
type OBOClient struct {
	id, secret string
	key        *rsa.PrivateKey
	kid        string
	tenant     string
	issuer     string // the server's issuer: the assertion's aud
}

// NewOBOClient creates an OBO client and registers a trusted key in tenant,
// both with tenant.Token, against baseURL.
func NewOBOClient(t *testing.T, baseURL string, tenant Tenant) *OBOClient {
	t.Helper()
	c := client.NewClient(baseURL, tenant.Token)
	code, body, err := c.CreateClientRaw(t, false, true)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create OBO client: %d %v", code, err)
	}
	var cred struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &cred); err != nil || cred.ID == "" || cred.Secret == "" {
		t.Fatalf("create OBO client: no credentials in the response (%v)", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	kid := "parity-obo-" + uuid.NewString()[:8]
	jwk := trustedJWKOf(&key.PublicKey, kid)
	if code, body, err := c.RegisterTrustedKeyRaw(t, map[string]any{"keyId": kid, "jwk": jwk}); err != nil || code != http.StatusOK {
		t.Fatalf("register trusted key: %d %v %s", code, err, body)
	}
	issuer, _ := tokenClaims(t, tenant.Token)["iss"].(string)
	if issuer == "" {
		t.Fatal("the tenant token carries no iss")
	}
	return &OBOClient{id: cred.ID, secret: cred.Secret, key: key, kid: kid, tenant: tenant.ID, issuer: issuer}
}

// assertion is a valid user assertion for user, signed with o's key.
func (o *OBOClient) assertion(t *testing.T, user string) string {
	t.Helper()
	now := time.Now()
	tok, err := auth.Sign(context.Background(), map[string]any{
		"sub": user, "iss": "parity-app", "aud": o.issuer, "caas_org_id": o.tenant,
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": uuid.NewString(),
		// Roles in the assertion never reach the issued token.
		"user_roles": []string{"ROLE_ADMIN"},
	}, auth.NewRSASigner(o.key), o.kid)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return tok
}

// KID is the key id of o's trusted key.
func (o *OBOClient) KID() string { return o.kid }

// Exchange runs the token exchange for user against baseURL as o and returns
// the status and body. A 200 body carries a token: never log it.
func (o *OBOClient) Exchange(t *testing.T, baseURL, user string) (int, []byte) {
	t.Helper()
	code, body, err := client.NewClient(baseURL, "").ExchangeTokenRaw(t, o.id, o.secret,
		url.Values{"subject_token": {o.assertion(t, user)}})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	return code, body
}

// OBOToken is an on-behalf-of token for user in tenant: it creates an OBO
// client and a trusted key in tenant with tenant.Token, signs an assertion
// for user and exchanges it. Any failure fails the test. The token is a
// credential: never log it.
func OBOToken(t *testing.T, fixture BackendFixture, tenant Tenant, user string) string {
	t.Helper()
	o := NewOBOClient(t, fixture.BaseURL(), tenant)
	code, body := o.Exchange(t, fixture.BaseURL(), user)
	if code != http.StatusOK {
		t.Fatalf("exchange: %d %s", code, body)
	}
	return accessToken(t, body)
}

// RunOBOExchange: an OBO client of a fresh tenant exchanges an assertion for
// alice; the token carries alice, the client as act.sub and the client's roles
// only; a plain client of the same tenant is refused the exchange.
func RunOBOExchange(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	tenant := fixture.NewTenant(t)
	o := NewOBOClient(t, base, tenant)

	code, body := o.Exchange(t, base, "alice")
	if code != http.StatusOK {
		t.Fatalf("exchange: %d %s", code, body)
	}
	tok := accessToken(t, body)
	claims := tokenClaims(t, tok)
	if claims["sub"] != "alice" || claims["caas_user_id"] != "alice" || claims["caas_org_id"] != tenant.ID {
		t.Errorf("sub %v caas_user_id %v caas_org_id %v, want alice, alice, %s",
			claims["sub"], claims["caas_user_id"], claims["caas_org_id"], tenant.ID)
	}
	if act, _ := claims["act"].(map[string]any); len(act) != 1 || act["sub"] != o.id {
		t.Errorf("act = %v, want exactly {sub: %s}", claims["act"], o.id)
	}
	if scopes, _ := claims["scopes"].([]any); len(scopes) != 1 || scopes[0] != "ROLE_M2M" {
		t.Errorf("scopes = %v, want the client's [ROLE_M2M]", claims["scopes"])
	}
	for _, absent := range []string{"user_roles", "cgen"} {
		if _, has := claims[absent]; has {
			t.Errorf("%s present on the OBO token", absent)
		}
	}
	if code, body, err := client.NewClient(base, tok).ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("the OBO token on GET /api/account: %d %v %s", code, err, body)
	}

	// A plain client of the same tenant is refused before its assertion is read.
	pc := client.NewClient(base, tenant.Token)
	pcode, pbody, err := pc.CreateClientRaw(t, false, false)
	if err != nil || pcode != http.StatusOK {
		t.Fatalf("create plain client: %d %v", pcode, err)
	}
	var plain struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if err := json.Unmarshal(pbody, &plain); err != nil || plain.ID == "" {
		t.Fatalf("create plain client: no credentials in the response (%v)", err)
	}
	code, body, err = client.NewClient(base, "").ExchangeTokenRaw(t, plain.ID, plain.Secret,
		url.Values{"subject_token": {o.assertion(t, "alice")}})
	if err != nil {
		t.Fatalf("plain client exchange: %v", err)
	}
	if code != http.StatusBadRequest || !strings.Contains(string(body), `"unauthorized_client"`) {
		t.Fatalf("plain client exchange: %d %s, want 400 unauthorized_client", code, body)
	}
}

// accessToken returns the access_token of a 200 token response.
func accessToken(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		t.Fatalf("no access_token in the token response (%v)", err)
	}
	return out.AccessToken
}

// tokenClaims decodes a JWT's claims without verifying it.
func tokenClaims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}
