package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingVault struct {
	KeyVault
	opens atomic.Int32
}

func (c *countingVault) Open(ctx context.Context, m KeyMeta, sealed []byte) (Signer, error) {
	c.opens.Add(1)
	return c.KeyVault.Open(ctx, m, sealed)
}

func issuedRecord(t *testing.T, v KeyVault, kid, aud string) []byte {
	t.Helper()
	meta := KeyMeta{KID: kid, Audience: aud, Algorithm: "RS256", Owner: v.Owner()}
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	vt := time.Now().Add(time.Hour)
	b, err := encodeSigningRecord(signingRecord{
		Kind: recordKindIssued, KID: kid, Audience: aud, Algorithm: "RS256", Active: true,
		ValidFrom: fmtTime(time.Now()), ValidTo: fmtTimePtr(&vt),
		PublicKey: base64.StdEncoding.EncodeToString(spki),
		Vault:     &vaultReference{Kind: v.Kind(), Owner: v.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testClassifier(t *testing.T) (*classifier, *countingVault) {
	t.Helper()
	v, err := NewWrappedVault(loadFixtureKey(t), "boot-kid")
	if err != nil {
		t.Fatal(err)
	}
	cv := &countingVault{KeyVault: v}
	return newClassifier(cv, "boot-kid"), cv
}

func TestClassify_Owned(t *testing.T) {
	c, v := testClassifier(t)
	e := c.classify(context.Background(), "k1", issuedRecord(t, v, "k1", "client"))
	if e.class != classOwned || e.signer == nil || e.pair.PublicKey == nil || e.pair.Audience != "client" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestClassify_SignerOpenedOnceWhileSealedUnchanged(t *testing.T) {
	c, v := testClassifier(t)
	rec := issuedRecord(t, v, "k1", "client")
	c.classify(context.Background(), "k1", rec)
	c.classify(context.Background(), "k1", rec)
	if n := v.opens.Load(); n != 1 {
		t.Fatalf("opened %d times, want 1", n)
	}
}

// The sealed bytes changing, with every other bound field (KID, audience,
// algorithm, owner, SPKI) held identical, must force a fresh Open rather than
// reuse the cached signer: a warm cache entry for the untampered record must
// not paper over sealed bytes that no longer decrypt. If the fingerprint
// dropped the sealed bytes, the second classify would still hit the cache
// and wrongly report owned.
func TestClassify_SignerReopensWhenSealedChanges(t *testing.T) {
	c, v := testClassifier(t)
	good := issuedRecord(t, v, "k1", "client")
	if e := c.classify(context.Background(), "k1", good); e.class != classOwned {
		t.Fatalf("setup: %+v", e)
	}

	var r signingRecord
	if err := json.Unmarshal(good, &r); err != nil {
		t.Fatal(err)
	}
	s, err := base64.StdEncoding.DecodeString(r.Vault.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	s[len(s)-1] ^= 1
	r.Vault.Sealed = base64.StdEncoding.EncodeToString(s)
	b, err := encodeSigningRecord(r)
	if err != nil {
		t.Fatal(err)
	}

	e := c.classify(context.Background(), "k1", b)
	if e.class != classBroken || !strings.Contains(e.reason, "decryption") {
		t.Fatalf("tampered sealed after cache fill: class=%v reason=%q, want broken/decryption", e.class, e.reason)
	}
}

// The cache must not treat two records as the same signer merely because
// their sealed bytes match: any bound field changing (publicKey, audience,
// algorithm, owner) must also force a fresh Open. A cache keyed on KID +
// sealed-hash alone would wrongly keep reporting the first record's class
// (owned) after a rewrite that only changes a bound field.
func TestClassify_CacheChecksEveryBoundFieldNotJustSealedBytes(t *testing.T) {
	c, v := testClassifier(t)
	good := issuedRecord(t, v, "k1", "client")
	if e := c.classify(context.Background(), "k1", good); e.class != classOwned {
		t.Fatalf("setup: class = %v, want owned", e.class)
	}

	var base signingRecord
	if err := json.Unmarshal(good, &base); err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherSPKI, err := x509.MarshalPKIXPublicKey(&otherKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(r *signingRecord){
		"different publicKey, same sealed bytes": func(r *signingRecord) {
			r.PublicKey = base64.StdEncoding.EncodeToString(otherSPKI)
		},
		"different audience, same sealed bytes": func(r *signingRecord) {
			r.Audience = "human"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			b, err := encodeSigningRecord(r)
			if err != nil {
				t.Fatal(err)
			}
			e := c.classify(context.Background(), "k1", b)
			if e.class != classBroken {
				t.Fatalf("class = %v, want broken (a cache hit must not skip the vault's binding check)", e.class)
			}
		})
	}
}

func TestClassify_Retired(t *testing.T) {
	c, _ := testClassifier(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := NewWrappedVault(other, "other-boot")
	if err != nil {
		t.Fatal(err)
	}
	if e := c.classify(context.Background(), "k1", issuedRecord(t, ov, "k1", "client")); e.class != classRetired {
		t.Fatalf("class = %v, want retired", e.class)
	}
}

func TestClassify_BrokenReasons(t *testing.T) {
	c, v := testClassifier(t)
	good := issuedRecord(t, v, "k1", "client")
	mutate := func(fn func(r *signingRecord)) []byte {
		var r signingRecord
		if err := json.Unmarshal(good, &r); err != nil {
			t.Fatal(err)
		}
		fn(&r)
		b, err := encodeSigningRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := map[string][]byte{
		"decryption": mutate(func(r *signingRecord) {
			s, err := base64.StdEncoding.DecodeString(r.Vault.Sealed)
			if err != nil {
				t.Fatal(err)
			}
			s[len(s)-1] ^= 1
			r.Vault.Sealed = base64.StdEncoding.EncodeToString(s)
		}),
		"unknown vault kind": mutate(func(r *signingRecord) { r.Vault.Kind = "kms-x" }),
	}
	for want, rec := range cases {
		e := c.classify(context.Background(), "k1", rec)
		if e.class != classBroken || !strings.Contains(e.reason, want) {
			t.Fatalf("%s: entry = %+v", want, e)
		}
	}
}

// The vault's post-decryption public-key check (associated data matched,
// but the sealed private key's own public key differs from the record's
// publicKey field) must surface through classify as broken, not just at the
// vault layer.
func TestClassify_BrokenPublicKeyMismatch(t *testing.T) {
	v, err := NewWrappedVault(loadFixtureKey(t), "boot-kid")
	if err != nil {
		t.Fatal(err)
	}
	c := newClassifier(v, "boot-kid")
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

	meta := KeyMeta{KID: "k1", Audience: "client", Algorithm: "RS256", Owner: v.Owner(), SPKI: spkiA}
	sealed := wv.aead.Seal(nil, nil, pk8B, associatedData(meta))

	rec, err := encodeSigningRecord(signingRecord{
		Kind: recordKindIssued, KID: "k1", Audience: "client", Algorithm: "RS256", Active: true,
		ValidFrom: fmtTime(time.Now()),
		PublicKey: base64.StdEncoding.EncodeToString(spkiA),
		Vault:     &vaultReference{Kind: v.Kind(), Owner: v.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
	})
	if err != nil {
		t.Fatal(err)
	}

	e := c.classify(context.Background(), "k1", rec)
	if e.class != classBroken || !strings.Contains(e.reason, "public key mismatch") {
		t.Fatalf("entry = %+v", e)
	}
}

func TestClassify_BootstrapAndForeign(t *testing.T) {
	c, _ := testClassifier(t)
	vt := time.Now().Add(time.Hour)

	own, err := encodeSigningRecord(signingRecord{
		Kind: recordKindBootstrap, KID: "boot-kid", Active: false,
		ValidFrom: fmtTime(time.Time{}), ValidTo: fmtTimePtr(&vt), Deleted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := c.classify(context.Background(), "boot-kid", own)
	if e.class != classBootstrapState || !e.deleted {
		t.Fatalf("own bootstrap = %+v", e)
	}
	if !e.pair.Bootstrap || e.pair.Active || !e.pair.ValidFrom.Equal(time.Time{}) ||
		e.pair.ValidTo == nil || !e.pair.ValidTo.Equal(vt) {
		t.Fatalf("own bootstrap pair = %+v", e.pair)
	}

	other, err := encodeSigningRecord(signingRecord{Kind: recordKindBootstrap, KID: "old-boot", Active: true, ValidFrom: fmtTime(time.Time{})})
	if err != nil {
		t.Fatal(err)
	}
	e2 := c.classify(context.Background(), "old-boot", other)
	if e2.class != classForeignBootstrap {
		t.Fatalf("foreign bootstrap = %+v", e2)
	}
	if !e2.pair.Bootstrap || !e2.pair.Active || !e2.pair.ValidFrom.Equal(time.Time{}) || e2.pair.ValidTo != nil {
		t.Fatalf("foreign bootstrap pair = %+v", e2.pair)
	}
}

// An issued record can never legitimately sit at the bootstrap KID: the
// bootstrap KID is reserved for bootstrap-state records. Trusting it as
// owned (it would otherwise pass every issued-record check, since its vault
// owner is this node's own bootstrap KID) would mean two keys under one KID.
func TestClassify_IssuedAtBootstrapKID(t *testing.T) {
	c, v := testClassifier(t)
	rec := issuedRecord(t, v, "boot-kid", "client")
	e := c.classify(context.Background(), "boot-kid", rec)
	if e.class != classUndecodable || e.reason != "issued record at the bootstrap key id" || e.pair.KID != "boot-kid" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestClassify_Undecodable(t *testing.T) {
	c, v := testClassifier(t)

	goodIssued := issuedRecord(t, v, "k1", "client")
	var base signingRecord
	if err := json.Unmarshal(goodIssued, &base); err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(r *signingRecord)) []byte {
		r := base
		if r.Vault != nil {
			vaultCopy := *r.Vault
			r.Vault = &vaultCopy
		}
		fn(&r)
		b, err := encodeSigningRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSPKI, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	invalidTime := "not-a-time"

	cases := map[string][]byte{
		"not json":             []byte("{"),
		"kid mismatch":         issuedRecord(t, v, "other", "client"),
		"bad kind":             []byte(`{"kind":"x","kid":"k1","validFrom":"2026-01-01T00:00:00Z"}`),
		"invalid audience":     mutate(func(r *signingRecord) { r.Audience = "admin" }),
		"non-RS256 algorithm":  mutate(func(r *signingRecord) { r.Algorithm = "RS512" }),
		"nil vault":            mutate(func(r *signingRecord) { r.Vault = nil }),
		"empty vault kind":     mutate(func(r *signingRecord) { r.Vault.Kind = "" }),
		"empty vault owner":    mutate(func(r *signingRecord) { r.Vault.Owner = "" }),
		"bad publicKey base64": mutate(func(r *signingRecord) { r.PublicKey = "not-base64!!!" }),
		"garbage SPKI": mutate(func(r *signingRecord) {
			r.PublicKey = base64.StdEncoding.EncodeToString([]byte("not a valid SPKI DER at all"))
		}),
		"non-RSA SPKI": mutate(func(r *signingRecord) {
			r.PublicKey = base64.StdEncoding.EncodeToString(ecSPKI)
		}),
		"bad sealed base64": mutate(func(r *signingRecord) { r.Vault.Sealed = "not-base64!!!" }),
		"invalid validFrom": mutate(func(r *signingRecord) { r.ValidFrom = invalidTime }),
		"invalid validTo":   mutate(func(r *signingRecord) { r.ValidTo = &invalidTime }),
		"bootstrap record with invalid validFrom": func() []byte {
			b, err := encodeSigningRecord(signingRecord{Kind: recordKindBootstrap, KID: "k1", Active: true, ValidFrom: invalidTime})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if e := c.classify(context.Background(), "k1", data); e.class != classUndecodable || e.pair.KID != "k1" {
				t.Fatalf("entry = %+v", e)
			}
		})
	}
}

// decodeSigningRecord must distinguish "not a public key at all" from
// "a public key of the wrong type", even though classify collapses both to
// the same undecodable class: a future caller of decodeSigningRecord itself
// needs the more specific reason.
func TestDecodeSigningRecord_PublicKeyErrors(t *testing.T) {
	_, v := testClassifier(t)
	good := issuedRecord(t, v, "k1", "client")
	var base signingRecord
	if err := json.Unmarshal(good, &base); err != nil {
		t.Fatal(err)
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSPKI, err := x509.MarshalPKIXPublicKey(&ecKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	garbage := base
	garbage.PublicKey = base64.StdEncoding.EncodeToString([]byte("not a valid SPKI DER at all"))
	garbageBytes, err := encodeSigningRecord(garbage)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := decodeSigningRecord("k1", garbageBytes); err == nil || !strings.Contains(err.Error(), "invalid public key") {
		t.Fatalf("err = %v, want %q", err, "invalid public key")
	}

	notRSA := base
	notRSA.PublicKey = base64.StdEncoding.EncodeToString(ecSPKI)
	notRSABytes, err := encodeSigningRecord(notRSA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := decodeSigningRecord("k1", notRSABytes); err == nil || !strings.Contains(err.Error(), "public key is not RSA") {
		t.Fatalf("err = %v, want %q", err, "public key is not RSA")
	}
}

// Review focus: a record written by a later version with extra fields.
func TestDecodeSigningRecord_IgnoresUnknownFields(t *testing.T) {
	_, v := testClassifier(t)
	var m map[string]any
	if err := json.Unmarshal(issuedRecord(t, v, "k1", "client"), &m); err != nil {
		t.Fatal(err)
	}
	m["futureField"] = map[string]any{"x": 1}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := decodeSigningRecord("k1", b); err != nil {
		t.Fatalf("unknown field made the record undecodable: %v", err)
	}
}

func TestSignerCache_Retain(t *testing.T) {
	sc := &signerCache{m: map[string]cachedSigner{}}
	sc.put("k1", [32]byte{1}, nil)
	sc.put("k2", [32]byte{2}, nil)

	sc.retain(map[string]bool{"k1": true})

	if _, ok := sc.m["k1"]; !ok {
		t.Fatal("retain dropped a KID that was in the set")
	}
	if _, ok := sc.m["k2"]; ok {
		t.Fatal("retain kept a KID that was not in the set")
	}
}
