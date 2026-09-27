package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
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
	"strings"
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

// requireUnsealReason asserts err is ErrUnseal AND names the given reason
// class. errors.Is alone would still pass if Open collapsed every distinct
// failure into one indistinguishable reason, so every failure test checks
// the reason text too.
func requireUnsealReason(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, ErrUnseal) {
		t.Fatalf("err = %v, want ErrUnseal", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("err = %v, want reason %q", err, reason)
	}
}

// asWrappedVault gives a test direct access to the vault's AEAD, to seal
// payloads the exported API can never produce (a mismatched key, a
// non-RSA key, or garbage) so the post-decryption checks in Open can be
// exercised directly.
func asWrappedVault(t *testing.T, v KeyVault) *wrappedVault {
	t.Helper()
	wv, ok := v.(*wrappedVault)
	if !ok {
		t.Fatalf("vault is %T, not *wrappedVault", v)
	}
	return wv
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
		v, err := NewWrappedVault(key, "owner-kid")
		if err != nil {
			t.Fatal(err)
		}
		meta := KeyMeta{KID: "golden-kid", Audience: "client", Algorithm: "RS256", Owner: "owner-kid"}
		spki, sealed, _, err := v.Generate(context.Background(), meta)
		if err != nil {
			t.Fatal(err)
		}
		meta.SPKI = spki
		g := vaultGolden{WrappingKeyHex: hex.EncodeToString(wk), Meta: meta, SealedHex: hex.EncodeToString(sealed)}
		b, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, '\n')
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
	v, err := NewWrappedVault(key, "owner-kid")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := hex.DecodeString(g.SealedHex)
	if err != nil {
		t.Fatal(err)
	}
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
	a, err := deriveWrappingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := deriveWrappingKey(again)
	if err != nil {
		t.Fatal(err)
	}
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
	a, err := deriveWrappingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := deriveWrappingKey(&reordered)
	if err != nil {
		t.Fatal(err)
	}
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

// Every bound field must stop the sealed key opening when it changes. All
// four are authenticated as AEAD associated data, so any change fails
// decryption itself, before the key is ever parsed.
func TestWrappedVault_AssociatedDataBindsEachField(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherSPKI, err := x509.MarshalPKIXPublicKey(&other.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
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
			_, err := v.Open(context.Background(), m, sealed)
			requireUnsealReason(t, err, "decryption")
		})
	}
}

func TestWrappedVault_RefusesOtherOwner(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI, meta.Owner = spki, "another-owner"
	_, err = v.Open(context.Background(), meta, sealed)
	requireUnsealReason(t, err, "owner mismatch")
}

// The owner is bound cryptographically, not only pre-checked: a second
// vault built from the SAME bootstrap key but a different owner passes the
// early owner check when meta.Owner is set to match ITS OWN owner, yet
// still cannot open a record sealed under the first vault's owner, because
// Owner is authenticated as associated data.
func TestWrappedVault_OwnerBoundThroughAEAD(t *testing.T) {
	v, meta := newTestVault(t) // owner "owner-kid"
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki

	other, err := NewWrappedVault(loadFixtureKey(t), "other")
	if err != nil {
		t.Fatal(err)
	}
	meta.Owner = "other" // matches other's own owner: passes the pre-check
	_, err = other.Open(context.Background(), meta, sealed)
	requireUnsealReason(t, err, "decryption")
}

func TestWrappedVault_OtherBootstrapKeyCannotOpen(t *testing.T) {
	v, meta := newTestVault(t)
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.SPKI = spki
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWrappedVault(otherKey, "owner-kid")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Open(context.Background(), meta, sealed)
	requireUnsealReason(t, err, "decryption")
}

// The public-key check runs only after decryption succeeds: sealing key B's
// private key directly under associated data that names key A's SPKI makes
// Open decrypt cleanly and then catch the swap in the post-decryption
// comparison.
func TestWrappedVault_PublicKeyMismatchAfterDecryption(t *testing.T) {
	v, meta := newTestVault(t)
	wv := asWrappedVault(t, v)

	keyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pk8B, err := x509.MarshalPKCS8PrivateKey(keyB)
	if err != nil {
		t.Fatal(err)
	}
	keyA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	spkiA, err := x509.MarshalPKIXPublicKey(&keyA.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	meta.SPKI = spkiA
	sealed := wv.aead.Seal(nil, nil, pk8B, associatedData(meta))
	_, err = v.Open(context.Background(), meta, sealed)
	requireUnsealReason(t, err, "public key mismatch")
}

// A non-RSA key parses successfully as PKCS#8 but fails the *rsa.PrivateKey
// type assertion; sealing it directly (bypassing Generate, which only ever
// produces RSA keys) reaches that branch.
func TestWrappedVault_NotRSAAfterDecryption(t *testing.T) {
	v, meta := newTestVault(t)
	wv := asWrappedVault(t, v)

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk8, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}

	// The RSA type assertion fails before SPKI is ever compared, so its
	// value only needs to match what was sealed for decryption to succeed.
	meta.SPKI = []byte("placeholder-spki")
	sealed := wv.aead.Seal(nil, nil, pk8, associatedData(meta))
	_, err = v.Open(context.Background(), meta, sealed)
	requireUnsealReason(t, err, "not RSA")
}

// Bytes that are not a valid PKCS#8 DER encoding at all must be reported as
// a distinct reason from "not RSA" (a valid PKCS#8 key of the wrong type).
func TestWrappedVault_NotPKCS8AfterDecryption(t *testing.T) {
	v, meta := newTestVault(t)
	wv := asWrappedVault(t, v)

	meta.SPKI = []byte("placeholder-spki")
	garbage := []byte("not a valid PKCS#8 DER-encoded private key")
	sealed := wv.aead.Seal(nil, nil, garbage, associatedData(meta))
	_, err := v.Open(context.Background(), meta, sealed)
	requireUnsealReason(t, err, "not PKCS#8")
}
