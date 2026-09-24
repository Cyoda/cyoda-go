package e2e_test

import (
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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// registerTrustedSigner generates an RSA key, registers its public half as a
// trusted key for the bootstrap tenant, and returns the private half and kid
// so a test can sign subject tokens the token-exchange grant will verify.
func registerTrustedSigner(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	kid := fmt.Sprintf("e2e-tx-%d", time.Now().UnixNano())
	jwk := map[string]any{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes()),
	}
	resp := adminRequest(t, "POST", "/oauth/keys/trusted",
		mustJSON(t, map[string]any{"keyId": kid, "jwk": jwk, "audience": "human"}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register trusted key: status=%d, want 200; body: %s", resp.StatusCode, raw)
	}
	// The tenant's trusted-key cap is shared by every test in the run.
	t.Cleanup(func() {
		del := adminRequest(t, "DELETE", "/oauth/keys/trusted/"+kid, nil)
		defer del.Body.Close()
		if del.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(del.Body)
			t.Errorf("delete trusted key %s: status=%d; body: %s", kid, del.StatusCode, raw)
		}
	})
	return priv, kid
}

// exchangeSubject presents a subject token carrying sub and tenant, signed by
// the trusted key, to the token-exchange grant as the bootstrap client.
func exchangeSubject(t *testing.T, priv *rsa.PrivateKey, kid, sub, tenant string) *http.Response {
	t.Helper()
	now := time.Now()
	subject, err := auth.Sign(map[string]any{
		"sub":         sub,
		"caas_org_id": tenant,
		"user_roles":  []string{"ROLE_USER"},
		"exp":         now.Add(time.Hour).Unix(),
		"iat":         now.Unix(),
		"jti":         uuid.NewString(),
	}, priv, kid)
	if err != nil {
		t.Fatalf("sign subject token: %v", err)
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {subject},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
	}
	return postToken(t, form, "testclient", "testsecret")
}

// TestToken_TokenExchange_KeyFromAnotherTenant_400: a trusted key belongs to
// the tenant that registered it. A client of another tenant cannot exchange a
// subject token signed with it, even one whose caas_org_id names the client's
// own tenant and asks for ROLE_ADMIN.
func TestToken_TokenExchange_KeyFromAnotherTenant_400(t *testing.T) {
	priv, kid := registerTrustedSigner(t) // registered in test-tenant
	otherTenant := fmt.Sprintf("e2e-tx-other-%d", time.Now().UnixNano())
	clientID, secret := createM2MClient(t, otherTenant, "other-m2m", []string{"ROLE_M2M"})

	now := time.Now()
	subject, err := auth.Sign(map[string]any{
		"sub":         "ext-user-1",
		"caas_org_id": otherTenant,
		"user_roles":  []string{"ROLE_ADMIN"},
		"exp":         now.Add(time.Hour).Unix(),
		"iat":         now.Unix(),
		"jti":         uuid.NewString(),
	}, priv, kid)
	if err != nil {
		t.Fatalf("sign subject token: %v", err)
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {subject},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
	}
	resp := postToken(t, form, clientID, secret)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body: %s", resp.StatusCode, raw)
	}
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode: %v; body: %s", err, raw)
	}
	// The description pins the reason: the key is not found in this client's
	// tenant, not some other rejection of the same token.
	if e.Error != "invalid_grant" || e.ErrorDescription != "unknown trusted key" {
		t.Errorf("got %+v, want invalid_grant / unknown trusted key", e)
	}
}

// TestToken_TokenExchange_Accepted: a subject token signed by a registered
// trusted key, for the client's own tenant, is exchanged for a token that the
// server then accepts.
func TestToken_TokenExchange_Accepted(t *testing.T) {
	priv, kid := registerTrustedSigner(t)

	resp := exchangeSubject(t, priv, kid, "ext-user-1", "test-tenant")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body: %s", resp.StatusCode, raw)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("no access_token in %s (err %v)", raw, err)
	}

	use := unauthRequest(t, http.MethodGet, "/api/model/", "Bearer "+tok.AccessToken)
	defer use.Body.Close()
	if use.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(use.Body)
		t.Fatalf("the exchanged token was rejected; body: %s", body)
	}
}

// TestToken_TokenExchange_TenantMismatch_403: a subject token for another
// tenant than the exchanging client's is refused.
func TestToken_TokenExchange_TenantMismatch_403(t *testing.T) {
	priv, kid := registerTrustedSigner(t)

	resp := exchangeSubject(t, priv, kid, "ext-user-1", "other-tenant")
	assertOAuthError(t, resp, http.StatusForbidden, "access_denied")
}

// TestToken_TokenExchange_InvalidSub_400: the exchanged token would carry the
// subject's sub as its user id, so a sub outside the user-id check is refused
// at the grant, and the response does not repeat it.
func TestToken_TokenExchange_InvalidSub_400(t *testing.T) {
	priv, kid := registerTrustedSigner(t)

	for name, sub := range map[string]string{
		"newline":  "ext\ninjected",
		"too-long": strings.Repeat("u", 256),
		"reserved": "oidc:11111111-2222-3333-4444-555555555555:injected",
	} {
		t.Run(name, func(t *testing.T) {
			resp := exchangeSubject(t, priv, kid, sub, "test-tenant")
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body: %s", resp.StatusCode, raw)
			}
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &e)
			if e.Error != "invalid_grant" {
				t.Errorf("error=%q, want invalid_grant; body: %s", e.Error, raw)
			}
			if strings.Contains(string(raw), "injected") || strings.Contains(string(raw), "uuuu") {
				t.Errorf("response echoes the rejected sub: %s", raw)
			}
		})
	}
}
