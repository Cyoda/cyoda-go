package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// The shared server is started with cfg.IAM.JWTExpiry = 3600 (the new
// maximum CYODA_JWT_EXPIRY_SECONDS accepts; see e2e_test.go TestMain). The
// default of 300 is covered by the unit tests in app/config_jwt_test.go and
// cmd/cyoda/token_test.go, which exercise LoadJWTSettings and `cyoda token`
// directly rather than a running server.

// TestToken_ClientCredentials_LifetimeIsConfiguredMax: a client_credentials
// token's lifetime is exactly the server's configured maximum (3600 s), and
// expires_in reports that remaining life.
func TestToken_ClientCredentials_LifetimeIsConfiguredMax(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	clientID, secret := createClient(t, false, false)
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, clientID, secret)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request: %d %s", resp.StatusCode, raw)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	if body.ExpiresIn < 3599 || body.ExpiresIn > 3600 {
		t.Fatalf("expires_in = %d, want in [3599, 3600]", body.ExpiresIn)
	}
	p, err := auth.Parse(body.AccessToken)
	if err != nil {
		t.Fatalf("parse access_token: %v", err)
	}
	exp, _ := p.Claims["exp"].(float64)
	iat, _ := p.Claims["iat"].(float64)
	if got := exp - iat; got != 3600 {
		t.Fatalf("exp - iat = %v, want 3600", got)
	}
}

// TestToken_Exchange_LifetimeCappedByAssertion: a token-exchange token's
// lifetime is capped by the assertion's own exp, not the server's (longer)
// configured maximum, per spec §4.4 (exp = min(assertion exp, now +
// CYODA_JWT_EXPIRY_SECONDS)).
func TestToken_Exchange_LifetimeCappedByAssertion(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const tenant = "test-tenant"
	oboID, oboSecret := createClient(t, false, true)
	priv, kid := registerTrustedSignerAs(t, suiteToken(t), "", nil)
	assertion := assertionFor(priv, kid, "alice", tenant, func(c map[string]any) {
		now := time.Now()
		c["iat"] = now.Unix()
		c["exp"] = now.Add(120 * time.Second).Unix()
	})
	resp := exchangeAs(t, oboID, oboSecret, assertion)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange: %d %s", resp.StatusCode, raw)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	if body.ExpiresIn < 119 || body.ExpiresIn > 120 {
		t.Fatalf("expires_in = %d, want in [119, 120]", body.ExpiresIn)
	}
	p, err := auth.Parse(body.AccessToken)
	if err != nil {
		t.Fatalf("parse access_token: %v", err)
	}
	exp, _ := p.Claims["exp"].(float64)
	iat, _ := p.Claims["iat"].(float64)
	if got := exp - iat; got < 119 || got > 120 {
		t.Fatalf("exp - iat = %v, want in [119, 120]", got)
	}
}
