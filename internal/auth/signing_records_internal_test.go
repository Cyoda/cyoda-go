package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
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
	v, _ := NewWrappedVault(loadFixtureKey(t), "boot-kid")
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

func TestClassify_Retired(t *testing.T) {
	c, _ := testClassifier(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	ov, _ := NewWrappedVault(other, "other-boot")
	if e := c.classify(context.Background(), "k1", issuedRecord(t, ov, "k1", "client")); e.class != classRetired {
		t.Fatalf("class = %v, want retired", e.class)
	}
}

func TestClassify_BrokenReasons(t *testing.T) {
	c, v := testClassifier(t)
	good := issuedRecord(t, v, "k1", "client")
	mutate := func(fn func(r *signingRecord)) []byte {
		var r signingRecord
		_ = json.Unmarshal(good, &r)
		fn(&r)
		b, _ := encodeSigningRecord(r)
		return b
	}
	cases := map[string][]byte{
		"decryption": mutate(func(r *signingRecord) {
			s, _ := base64.StdEncoding.DecodeString(r.Vault.Sealed)
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

func TestClassify_BootstrapAndForeign(t *testing.T) {
	c, _ := testClassifier(t)
	own, _ := encodeSigningRecord(signingRecord{Kind: recordKindBootstrap, KID: "boot-kid", Active: false, ValidFrom: fmtTime(time.Time{}), Deleted: true})
	if e := c.classify(context.Background(), "boot-kid", own); e.class != classBootstrapState || !e.deleted {
		t.Fatalf("own bootstrap = %+v", e)
	}
	other, _ := encodeSigningRecord(signingRecord{Kind: recordKindBootstrap, KID: "old-boot", Active: true, ValidFrom: fmtTime(time.Time{})})
	if e := c.classify(context.Background(), "old-boot", other); e.class != classForeignBootstrap {
		t.Fatalf("foreign bootstrap = %+v", e)
	}
}

func TestClassify_Undecodable(t *testing.T) {
	c, v := testClassifier(t)
	for name, data := range map[string][]byte{
		"not json":     []byte("{"),
		"kid mismatch": issuedRecord(t, v, "other", "client"),
		"bad kind":     []byte(`{"kind":"x","kid":"k1","validFrom":"2026-01-01T00:00:00Z"}`),
	} {
		if e := c.classify(context.Background(), "k1", data); e.class != classUndecodable || e.pair.KID != "k1" {
			t.Fatalf("%s: entry = %+v", name, e)
		}
	}
}

// Review focus: a record written by a later version with extra fields.
func TestDecodeSigningRecord_IgnoresUnknownFields(t *testing.T) {
	_, v := testClassifier(t)
	var m map[string]any
	_ = json.Unmarshal(issuedRecord(t, v, "k1", "client"), &m)
	m["futureField"] = map[string]any{"x": 1}
	b, _ := json.Marshal(m)
	if _, _, _, _, err := decodeSigningRecord("k1", b); err != nil {
		t.Fatalf("unknown field made the record undecodable: %v", err)
	}
}
