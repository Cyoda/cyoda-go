package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestMintOperatorToken_Claims(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Unix(1_900_000_000, 0)
	tok, err := auth.MintOperatorToken(context.Background(), key, auth.OperatorTokenRequest{
		Tenant: "acme", UserID: "operator", Roles: []string{"ROLE_ADMIN"},
		TTL: 15 * time.Minute, Issuer: "cyoda", Audience: "cyoda-api", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := auth.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	wantKID, _ := auth.DeriveKID(&key.PublicKey)
	if p.Header["kid"] != wantKID {
		t.Errorf("kid = %v, want %s", p.Header["kid"], wantKID)
	}
	for k, want := range map[string]any{"sub": "operator", "caas_user_id": "operator", "caas_org_id": "acme", "iss": "cyoda", "aud": "cyoda-api"} {
		if p.Claims[k] != want {
			t.Errorf("%s = %v, want %v", k, p.Claims[k], want)
		}
	}
	if roles, _ := p.Claims["user_roles"].([]any); len(roles) != 1 || roles[0] != "ROLE_ADMIN" {
		t.Errorf("user_roles = %v", p.Claims["user_roles"])
	}
	if _, has := p.Claims["scopes"]; has {
		t.Error("scopes must be absent: the principal is a person (user_roles)")
	}
	if exp, _ := p.Claims["exp"].(float64); int64(exp) != now.Add(15*time.Minute).Unix() {
		t.Errorf("exp = %v", p.Claims["exp"])
	}
	if jti, _ := p.Claims["jti"].(string); jti == "" {
		t.Error("jti missing")
	}
	if err := auth.Verify(p.SigningInput, p.Signature, &key.PublicKey); err != nil {
		t.Errorf("signature: %v", err)
	}
}

func TestMintOperatorToken_NoAudienceOmitsAud(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok, err := auth.MintOperatorToken(context.Background(), key, auth.OperatorTokenRequest{
		Tenant: "acme", UserID: "operator", Roles: []string{"ROLE_ADMIN"}, TTL: time.Minute, Issuer: "cyoda",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := auth.Parse(tok)
	if _, has := p.Claims["aud"]; has {
		t.Fatal("aud present")
	}
}

func TestMintOperatorToken_Refusals(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	ok := auth.OperatorTokenRequest{Tenant: "acme", UserID: "operator", Roles: []string{"ROLE_ADMIN"}, TTL: time.Minute, Issuer: "cyoda"}
	cases := map[string]func(r *auth.OperatorTokenRequest){
		"bad tenant":  func(r *auth.OperatorTokenRequest) { r.Tenant = spi.TenantID("a:b") },
		"empty user":  func(r *auth.OperatorTokenRequest) { r.UserID = "" },
		"system user": func(r *auth.OperatorTokenRequest) { r.UserID = "system" },
		"no roles":    func(r *auth.OperatorTokenRequest) { r.Roles = nil },
		"empty role":  func(r *auth.OperatorTokenRequest) { r.Roles = []string{"ROLE_ADMIN", ""} },
		"zero ttl":    func(r *auth.OperatorTokenRequest) { r.TTL = 0 },
		// iat and exp are whole seconds: a sub-second lifetime would give a
		// token that has expired when it is issued.
		"1ns ttl":        func(r *auth.OperatorTokenRequest) { r.TTL = time.Nanosecond },
		"sub-second ttl": func(r *auth.OperatorTokenRequest) { r.TTL = time.Second - time.Nanosecond },
		"no issuer":      func(r *auth.OperatorTokenRequest) { r.Issuer = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := ok
			mut(&r)
			if _, err := auth.MintOperatorToken(context.Background(), key, r); err == nil {
				t.Fatal("want error")
			}
		})
	}
	if _, err := auth.MintOperatorToken(context.Background(), nil, ok); err == nil {
		t.Fatal("nil key: want error")
	}
	for name, mut := range cases {
		r := ok
		mut(&r)
		if err := auth.ValidateOperatorTokenRequest(r); err == nil {
			t.Errorf("ValidateOperatorTokenRequest(%s): want error", name)
		}
	}
	oneSecond := ok
	oneSecond.TTL = time.Second
	if err := auth.ValidateOperatorTokenRequest(oneSecond); err != nil {
		t.Errorf("1s ttl refused: %v", err)
	}
	if err := auth.ValidateOperatorTokenRequest(ok); err != nil {
		t.Errorf("valid request refused: %v", err)
	}
}

func TestValidateOperatorTokenRequest_RefusesSystemTenant(t *testing.T) {
	for _, tenant := range []spi.TenantID{"SYSTEM", "system", "System"} {
		r := auth.OperatorTokenRequest{Tenant: tenant, UserID: "operator", Roles: []string{"ROLE_ADMIN"}, TTL: time.Minute, Issuer: "cyoda"}
		if err := auth.ValidateOperatorTokenRequest(r); !errors.Is(err, common.ErrReservedTenantID) {
			t.Errorf("tenant %q: err = %v, want ErrReservedTenantID", tenant, err)
		}
	}
}
