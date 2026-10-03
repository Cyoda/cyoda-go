package parity

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunTrustedKeyPerTenantKid: trusted-key ids are unique per tenant, on every
// backend's KV store. Two tenants register different keys under one kid;
// both succeed, each lists exactly its own key, and invalidating or deleting
// one tenant's key leaves the other tenant's key as it was.
func RunTrustedKeyPerTenantKid(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	b := fixture.NewTenant(t)
	ca := client.NewClient(fixture.BaseURL(), a.Token)
	cb := client.NewClient(fixture.BaseURL(), b.Token)
	const kid = "parity-same-kid"

	jwkA, jwkB := trustedJWK(t, kid), trustedJWK(t, kid)
	for name, c := range map[string]struct {
		cl  *client.Client
		jwk map[string]any
	}{"a": {ca, jwkA}, "b": {cb, jwkB}} {
		code, body, err := c.cl.RegisterTrustedKeyRaw(t, map[string]any{"keyId": kid, "jwk": c.jwk})
		if err != nil || code != http.StatusOK {
			t.Fatalf("register in tenant %s: %d %v %s", name, code, err, body)
		}
	}

	// own returns the tenant's keys with kid, as the list shows them.
	own := func(cl *client.Client) []map[string]any {
		t.Helper()
		code, body, err := cl.ListTrustedKeysRaw(t)
		if err != nil || code != http.StatusOK {
			t.Fatalf("list: %d %v %s", code, err, body)
		}
		var keys []map[string]any
		if err := json.Unmarshal(body, &keys); err != nil {
			t.Fatalf("decode list: %v; body: %s", err, body)
		}
		var out []map[string]any
		for _, k := range keys {
			if k["keyId"] == kid {
				out = append(out, k)
			}
		}
		return out
	}
	modulus := func(k map[string]any) string {
		jwk, _ := k["jwk"].(map[string]any)
		n, _ := jwk["n"].(string)
		return n
	}
	for name, c := range map[string]struct {
		cl  *client.Client
		jwk map[string]any
	}{"a": {ca, jwkA}, "b": {cb, jwkB}} {
		keys := own(c.cl)
		if len(keys) != 1 || modulus(keys[0]) != c.jwk["n"] {
			t.Fatalf("tenant %s lists %v under kid %s, want exactly its own key", name, keys, kid)
		}
		if _, has := keys[0]["audience"]; has {
			t.Errorf("tenant %s: trusted key carries an audience: %v", name, keys[0])
		}
	}

	if code, body, err := ca.InvalidateTrustedKeyRaw(t, kid); err != nil || code != http.StatusOK {
		t.Fatalf("invalidate in tenant a: %d %v %s", code, err, body)
	}
	if keys := own(cb); len(keys) != 1 || keys[0]["active"] != true {
		t.Fatalf("tenant b's key after tenant a's invalidate: %v, want it active", keys)
	}
	if code, body, err := ca.DeleteTrustedKeyRaw(t, kid); err != nil || code != http.StatusOK {
		t.Fatalf("delete in tenant a: %d %v %s", code, err, body)
	}
	if keys := own(ca); len(keys) != 0 {
		t.Fatalf("tenant a still lists %v after its delete", keys)
	}
	if keys := own(cb); len(keys) != 1 || modulus(keys[0]) != jwkB["n"] {
		t.Fatalf("tenant b's key after tenant a's delete: %v", keys)
	}
	if code, body, err := cb.DeleteTrustedKeyRaw(t, kid); err != nil || code != http.StatusOK {
		t.Fatalf("delete in tenant b: %d %v %s", code, err, body)
	}
}

// trustedJWK is the public JWK of a freshly generated RSA key, under kid.
func trustedJWK(t *testing.T, kid string) map[string]any {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return trustedJWKOf(&priv.PublicKey, kid)
}

// trustedJWKOf is the public JWK of pub, under kid.
func trustedJWKOf(pub *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
