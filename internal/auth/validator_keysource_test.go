package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// fakeKeySource records calls and returns a fixed key. Proves the validator
// goes through the KeySource interface rather than any HTTP client.
type fakeKeySource struct {
	key      *rsa.PublicKey
	kid      string
	getCalls int
}

func (f *fakeKeySource) GetKey(kid string) (*rsa.PublicKey, error) {
	f.getCalls++
	if kid != f.kid {
		return nil, auth.ErrKeyNotFound
	}
	return f.key, nil
}

func TestValidator_RoutesKeyLookupThroughInjectedSource(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}

	kid := "keysource-kid"
	fake := &fakeKeySource{key: &priv.PublicKey, kid: kid}

	v := auth.NewValidatorFromSource(fake, "test-issuer")

	now := time.Now().Unix()
	claims := map[string]any{
		"iss":          "test-issuer",
		"sub":          "u1",
		"caas_user_id": "u1",
		"caas_org_id":  "org1",
		"exp":          float64(now + 300),
		"iat":          float64(now),
	}
	token := signTestToken(t, priv, kid, claims)

	uc, _, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if uc == nil || uc.UserID != "u1" {
		t.Fatalf("unexpected user context: %+v", uc)
	}
	if fake.getCalls == 0 {
		t.Fatal("KeySource.GetKey was never invoked; validator bypassed the injected source")
	}
}
