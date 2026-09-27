package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"os"
	"testing"
)

var updateGolden = flag.Bool("update-vault-golden", false, "rewrite testdata/vault/golden.json")

func loadFixtureKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	raw, err := os.ReadFile("testdata/vault/bootstrap-pkcs8.pem")
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseRSAPrivateKeyFromPEM(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type vaultGolden struct {
	WrappingKeyHex string  `json:"wrappingKeyHex"`
	Meta           KeyMeta `json:"meta"`
	SealedHex      string  `json:"sealedHex"`
}

// The wrapping key and a sealed record are pinned: a change to the
// derivation or the sealing format would retire every stored key pair.
func TestWrappedVault_Golden(t *testing.T) {
	key := loadFixtureKey(t)
	wk, err := deriveWrappingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		v, _ := NewWrappedVault(key, "owner-kid")
		meta := KeyMeta{KID: "golden-kid", Audience: "client", Algorithm: "RS256", Owner: "owner-kid"}
		spki, sealed, _, err := v.Generate(context.Background(), meta)
		if err != nil {
			t.Fatal(err)
		}
		meta.SPKI = spki
		g := vaultGolden{WrappingKeyHex: hex.EncodeToString(wk), Meta: meta, SealedHex: hex.EncodeToString(sealed)}
		b, _ := json.MarshalIndent(g, "", "  ")
		if err := os.WriteFile("testdata/vault/golden.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/vault/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g vaultGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(wk) != g.WrappingKeyHex {
		t.Fatalf("wrapping key changed: got %x", wk)
	}
	v, _ := NewWrappedVault(key, "owner-kid")
	sealed, _ := hex.DecodeString(g.SealedHex)
	if _, err := v.Open(context.Background(), g.Meta, sealed); err != nil {
		t.Fatalf("golden sealed record no longer opens: %v", err)
	}
}

func TestWrappingKey_IndependentOfEncoding(t *testing.T) {
	key := loadFixtureKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	again, err := ParseRSAPrivateKeyFromPEM(pkcs1)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := deriveWrappingKey(key)
	b, _ := deriveWrappingKey(again)
	if !bytes.Equal(a, b) {
		t.Fatal("PKCS#1 and PKCS#8 encodings of one key derive different wrapping keys")
	}
}

func TestWrappingKey_MultiPrime(t *testing.T) {
	//nolint:staticcheck // multi-prime keys are deprecated but still parse
	key, err := rsa.GenerateMultiPrimeKey(rand.Reader, 3, 2048)
	if err != nil {
		t.Skipf("multi-prime generation unavailable: %v", err)
	}
	reordered := *key
	reordered.Primes = []*big.Int{key.Primes[2], key.Primes[0], key.Primes[1]}
	a, _ := deriveWrappingKey(key)
	b, _ := deriveWrappingKey(&reordered)
	if !bytes.Equal(a, b) {
		t.Fatal("prime order changes the wrapping key")
	}
}

func newTestVault(t *testing.T) (KeyVault, KeyMeta) {
	t.Helper()
	v, err := NewWrappedVault(loadFixtureKey(t), "owner-kid")
	if err != nil {
		t.Fatal(err)
	}
	return v, KeyMeta{KID: "k1", Audience: "client", Algorithm: "RS256", Owner: "owner-kid"}
}

func TestWrappedVault_RoundTrip(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, s, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki
	opened, err := v.Open(context.Background(), meta, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !opened.Public().(*rsa.PublicKey).Equal(s.Public()) {
		t.Fatal("opened key differs from generated key")
	}
}

// Every bound field must stop the sealed key opening when it changes.
func TestWrappedVault_AssociatedDataBindsEachField(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherSPKI, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	cases := map[string]func(m *KeyMeta){
		"kid":       func(m *KeyMeta) { m.KID = "k2" },
		"audience":  func(m *KeyMeta) { m.Audience = "human" },
		"algorithm": func(m *KeyMeta) { m.Algorithm = "RS512" },
		"spki":      func(m *KeyMeta) { m.SPKI = otherSPKI },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := meta
			mutate(&m)
			if _, err := v.Open(context.Background(), m, sealed); !errors.Is(err, ErrUnseal) {
				t.Fatalf("Open with changed %s: err = %v, want ErrUnseal", name, err)
			}
		})
	}
}

func TestWrappedVault_RefusesOtherOwner(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, _ := v.Generate(context.Background(), meta)
	meta.SPKI, meta.Owner = spki, "another-owner"
	if _, err := v.Open(context.Background(), meta, sealed); !errors.Is(err, ErrUnseal) {
		t.Fatalf("err = %v, want ErrUnseal", err)
	}
}

func TestWrappedVault_OtherBootstrapKeyCannotOpen(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, _ := v.Generate(context.Background(), meta)
	meta.SPKI = spki
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	w, _ := NewWrappedVault(otherKey, "owner-kid")
	if _, err := w.Open(context.Background(), meta, sealed); !errors.Is(err, ErrUnseal) {
		t.Fatalf("err = %v, want ErrUnseal", err)
	}
}
