package e2e_test

import (
	"context"
	"crypto/rsa"
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
// the server accepts it on HTTP and gRPC (gRPC serves it only with
// ROLE_M2M), refuses it once the signing key is invalidated, keeps accepting
// it across a rotation, and — with CYODA_JWT_AUDIENCE set — accepts it and
// the server's own issued tokens only when they carry that audience.

// operatorToken signs a token the way `cyoda token --tenant PLATFORM` does
// (auth.MintOperatorToken, its defaults: user operator, ROLE_ADMIN, 15
// minutes) for the PLATFORM tenant — a platform operator. audience "" omits
// aud.
func operatorToken(t *testing.T, key *rsa.PrivateKey, issuer, audience string) string {
	t.Helper()
	return operatorTokenWithRoles(t, key, issuer, audience, "ROLE_ADMIN")
}

// operatorTokenWithRoles is operatorToken signed with roles, the way
// `cyoda token --tenant PLATFORM --roles` does.
func operatorTokenWithRoles(t *testing.T, key *rsa.PrivateKey, issuer, audience string, roles ...string) string {
	t.Helper()
	tok, err := auth.MintOperatorToken(context.Background(), key, auth.OperatorTokenRequest{
		Tenant: auth.PlatformTenantID, UserID: "operator", Roles: roles,
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
	// The default roles carry no ROLE_M2M: the token authenticates on gRPC
	// but no method serves it. Signed with ROLE_M2M, it reaches data.
	if err := grpcEntitySearch(h, tok); status.Code(err) != codes.PermissionDenied {
		t.Errorf("gRPC with the default roles: %v, want PermissionDenied", err)
	}
	if err := grpcEntitySearch(h, operatorTokenWithRoles(t, key, "cyoda-callback-test", "", "ROLE_ADMIN", "ROLE_M2M")); err != nil {
		t.Errorf("gRPC refused a cyoda token token with ROLE_M2M: %v", err)
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
	h.invalidateKey(t, bootKID, 0)
	if code := h.authedStatus(t, tok); code != http.StatusUnauthorized {
		t.Errorf("HTTP after the invalidate: %d, want 401", code)
	}
	if err := grpcEntitySearch(h, tok); status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC after the invalidate: %v, want Unauthenticated", err)
	}
}

// TestCyodaToken_SurvivesRotation: a rotation (invalidateCurrent) ends the
// issued key pair it replaces, signs with the new one, and leaves the signing
// key alone — nothing is written for it, and a cyoda token token is still
// accepted.
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
	k1 := h.issueKey(t, false)
	t1 := h.oauthToken(t)
	if got := tokenKID(t, t1); got != k1 {
		t.Fatalf("before the rotation the server signs with %s, want %s", got, k1)
	}

	k2 := h.issueKey(t, true)
	if code, cur := h.currentKey(t); code != http.StatusOK || cur != k2 {
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
	rows := func(kid string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM kv_store WHERE tenant_id='SYSTEM' AND namespace='signing-keys' AND key=$1`, kid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := rows(k1); n != 1 {
		t.Fatalf("control: the replaced key pair has %d stored rows, want 1", n)
	}
	if n := rows(bootKID); n != 0 {
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

	// The OBO token is minted on this stack, whose admin tokens carry the
	// audience; the assertion's aud is the stack's issuer.
	exchanged := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if code := h.authedStatus(t, exchanged); code != http.StatusOK {
		t.Errorf("token-exchange token: %d, want 200", code)
	}

	if code := h.authedStatus(t, operatorToken(t, key, "cyoda-callback-test", audience)); code != http.StatusOK {
		t.Errorf("cyoda token token with aud: %d, want 200", code)
	}
	if code := h.authedStatus(t, operatorToken(t, key, "cyoda-callback-test", "")); code != http.StatusUnauthorized {
		t.Errorf("token without aud: %d, want 401", code)
	}
}
