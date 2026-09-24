package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func registerForVerification(t *testing.T, s auth.TrustedKeyStore, kid string, tenant spi.TenantID, validTo *time.Time) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := s.Register(&auth.TrustedKey{
		KID: kid, TenantID: tenant, PublicKey: &priv.PublicKey, Audience: "human",
		Active: validTo == nil, ValidFrom: time.Now().Add(-2 * time.Hour), ValidTo: validTo,
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("register %s: %v", kid, err)
	}
}

// assertGetForVerification is the contract both stores share: a key is found
// only in the tenant that registered it, and only while within its validity
// window; anything else is ErrTrustedKeyNotFound.
func assertGetForVerification(t *testing.T, s auth.TrustedKeyStore) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	registerForVerification(t, s, "k1", "ta", nil)
	registerForVerification(t, s, "expired", "ta", &past)

	got, err := s.GetForVerification("ta", "k1")
	if err != nil || got.KID != "k1" || got.TenantID != "ta" {
		t.Fatalf("owner tenant: got=%+v err=%v", got, err)
	}
	for name, c := range map[string]struct {
		tenant spi.TenantID
		kid    string
	}{
		"other-tenant": {"tb", "k1"},
		"expired":      {"ta", "expired"},
		"missing":      {"ta", "missing"},
	} {
		if _, err := s.GetForVerification(c.tenant, c.kid); !errors.Is(err, auth.ErrTrustedKeyNotFound) {
			t.Errorf("%s: err = %v, want ErrTrustedKeyNotFound", name, err)
		}
	}
}

func TestInMemoryTrustedKeyStore_GetForVerification(t *testing.T) {
	assertGetForVerification(t, auth.NewInMemoryTrustedKeyStore())
}

func TestKVTrustedKeyStore_GetForVerification(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	assertGetForVerification(t, s)
}
