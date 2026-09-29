package e2e_test

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cyoda_token_test.go proves, end to end against a real PostgreSQL, the
// admin token `cyoda token` signs offline with the configured signing key:
// the server accepts it on HTTP and gRPC, refuses it once the signing key is
// invalidated, keeps accepting it across a rotation, and — with
// CYODA_JWT_AUDIENCE set — accepts it and the server's own issued tokens only
// when they carry that audience.

// operatorToken signs a token the way `cyoda token` does (auth.MintOperatorToken,
// its defaults: user operator, ROLE_ADMIN, 15 minutes) for tenant test-tenant.
// audience "" omits aud.
func operatorToken(t *testing.T, key *rsa.PrivateKey, issuer, audience string) string {
	t.Helper()
	tok, err := auth.MintOperatorToken(context.Background(), key, auth.OperatorTokenRequest{
		Tenant: "test-tenant", UserID: "operator", Roles: []string{"ROLE_ADMIN"},
		TTL: 15 * time.Minute, Issuer: issuer, Audience: audience,
	})
	if err != nil {
		t.Fatalf("mint operator token: %v", err)
	}
	return tok
}

func TestCyodaToken_AcceptedOnHTTPAndUnaryGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	key := genKey(t)
	h := newKeyStackOn(t, newSchedDB(t), key)
	tok := operatorToken(t, key, "cyoda-callback-test", "")
	if code := h.authedStatus(t, tok); code != http.StatusOK {
		t.Errorf("HTTP: %d, want 200", code)
	}
	if err := grpcEntitySearch(h, tok); err != nil {
		t.Errorf("gRPC refused a cyoda token token: %v", err)
	}
}

func TestCyodaToken_RefusedAfterSigningKeyInvalidated(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	key := genKey(t)
	bootKID, err := auth.DeriveKID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newKeyStackOn(t, newSchedDB(t), key)
	tok := operatorToken(t, key, "cyoda-callback-test", "")
	if code := h.authedStatus(t, tok); code != http.StatusOK {
		t.Fatalf("control: before the invalidate: %d, want 200", code)
	}
	if code, b := h.keyCall(t, "POST", "/oauth/keys/keypair/"+bootKID+"/invalidate", `{"gracePeriodSec":0}`); code != http.StatusOK {
		t.Fatalf("invalidate the signing key: %d %s", code, b)
	}
	if code := h.authedStatus(t, tok); code != http.StatusUnauthorized {
		t.Errorf("HTTP after the invalidate: %d, want 401", code)
	}
	if err := grpcEntitySearch(h, tok); status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC after the invalidate: %v, want Unauthenticated", err)
	}
}

// TestCyodaToken_SurvivesRotation: a rotation (invalidateCurrent) on the
// signing key's audience ends the issued key pair it replaces, signs with the
// new one, and leaves the signing key alone — nothing is written for it, and a
// cyoda token token is still accepted.
func TestCyodaToken_SurvivesRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	bootKID, err := auth.DeriveKID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newKeyStackOn(t, s, key)
	k1 := h.issueKey(t, "client", false)
	t1 := h.oauthToken(t)
	if got := tokenKID(t, t1); got != k1 {
		t.Fatalf("before the rotation the server signs with %s, want %s", got, k1)
	}

	k2 := h.issueKey(t, "client", true)
	if code, cur := h.currentKey(t, "client"); code != http.StatusOK || cur != k2 {
		t.Errorf("current after the rotation: %d %s, want 200 %s", code, cur, k2)
	}
	if got := tokenKID(t, h.oauthToken(t)); got != k2 {
		t.Errorf("after the rotation the server signs with %s, want %s", got, k2)
	}
	if code := h.authedStatus(t, t1); code != http.StatusUnauthorized {
		t.Errorf("token of the replaced key pair: %d, want 401", code)
	}
	if code := h.authedStatus(t, operatorToken(t, key, "cyoda-callback-test", "")); code != http.StatusOK {
		t.Errorf("cyoda token token after the rotation: %d, want 200", code)
	}
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM kv_store WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$1`, bootKID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the rotation wrote a record for the signing key (%d rows), want none", n)
	}
}

// TestIssuedTokens_AcceptedWithConfiguredAudience: with CYODA_JWT_AUDIENCE
// set, the server accepts its own client_credentials and token-exchange
// tokens and a cyoda token token that carries the audience, and refuses a
// token without aud.
func TestIssuedTokens_AcceptedWithConfiguredAudience(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const audience = "cyoda-api"
	key := genKey(t)
	h := newKeyStackWith(t, newSchedDB(t), key, func(cfg *app.Config) {
		cfg.IAM.JWTAudience = audience
		cfg.IAM.TrustedKeyRegistrationEnabled = true
	})

	if code := h.authedStatus(t, h.oauthToken(t)); code != http.StatusOK {
		t.Errorf("client_credentials token: %d, want 200", code)
	}

	// The trusted key is registered with the harness's own admin token, which
	// carries the audience; that keeps the registration independent of what
	// /oauth/token issues.
	priv := genKey(t)
	const trustedKID = "e2e-aud-trusted"
	body, err := json.Marshal(trustedKeyBody(priv, trustedKID))
	if err != nil {
		t.Fatal(err)
	}
	resp := h.DoAuth(t, http.MethodPost, "/api/oauth/keys/trusted", string(body), "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register trusted key: %d %s", resp.StatusCode, raw)
	}
	exchanged := h.grantToken(t, exchangeForm(t, priv, trustedKID, "ext-user-1", "test-tenant", []string{"ROLE_USER"}), h.clientID, h.clientSecret)
	if code := h.authedStatus(t, exchanged); code == http.StatusUnauthorized {
		t.Errorf("token-exchange token: 401, want accepted")
	}

	if code := h.authedStatus(t, operatorToken(t, key, "cyoda-callback-test", audience)); code != http.StatusOK {
		t.Errorf("cyoda token token with aud: %d, want 200", code)
	}
	if code := h.authedStatus(t, operatorToken(t, key, "cyoda-callback-test", "")); code != http.StatusUnauthorized {
		t.Errorf("token without aud: %d, want 401", code)
	}
}
