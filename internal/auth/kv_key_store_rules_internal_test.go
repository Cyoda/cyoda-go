package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"
)

// issuedRecordFull builds an issued record with a fully specified window, for
// table tests that need active=false, a future validFrom, or an expiry —
// issuedRecord (signing_records_internal_test.go) always builds an
// active, currently-open one.
func issuedRecordFull(t *testing.T, v KeyVault, kid, aud string, active bool, from time.Time, to *time.Time) []byte {
	t.Helper()
	meta := KeyMeta{KID: kid, Audience: aud, Algorithm: "RS256", Owner: v.Owner()}
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeSigningRecord(signingRecord{
		Kind: recordKindIssued, KID: kid, Audience: aud, Algorithm: "RS256", Active: active,
		ValidFrom: fmtTime(from), ValidTo: fmtTimePtr(to),
		PublicKey: base64.StdEncoding.EncodeToString(spki),
		Vault:     &vaultReference{Kind: v.Kind(), Owner: v.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// brokenRecord builds an issued record this node's own vault owns but cannot
// open (corrupted sealed bytes) — classOwned's decryption-failure sibling.
// from/to are the case's own window, not a fixed one: a hardcoded window here
// would race the table's other records on wall-clock time as subtests run.
func brokenRecord(t *testing.T, v KeyVault, kid, aud string, from time.Time, to *time.Time) []byte {
	t.Helper()
	meta := KeyMeta{KID: kid, Audience: aud, Algorithm: "RS256", Owner: v.Owner()}
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte{}, sealed...)
	corrupted[0] ^= 0xFF
	b, err := encodeSigningRecord(signingRecord{
		Kind: recordKindIssued, KID: kid, Audience: aud, Algorithm: "RS256", Active: true,
		ValidFrom: fmtTime(from), ValidTo: fmtTimePtr(to),
		PublicKey: base64.StdEncoding.EncodeToString(spki),
		Vault:     &vaultReference{Kind: v.Kind(), Owner: v.Owner(), Sealed: base64.StdEncoding.EncodeToString(corrupted)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// unknownVaultKindRecord builds an issued record whose vault kind does not
// match this node's vault at all — never sealed under this bootstrap key's
// wrap scheme, regardless of the owner field.
func unknownVaultKindRecord(t *testing.T, v KeyVault, kid, aud string) []byte {
	t.Helper()
	meta := KeyMeta{KID: kid, Audience: aud, Algorithm: "RS256", Owner: v.Owner()}
	spki, sealed, _, err := v.Generate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Hour)
	b, err := encodeSigningRecord(signingRecord{
		Kind: recordKindIssued, KID: kid, Audience: aud, Algorithm: "RS256", Active: true,
		ValidFrom: fmtTime(from), ValidTo: fmtTimePtr(&to),
		PublicKey: base64.StdEncoding.EncodeToString(spki),
		Vault:     &vaultReference{Kind: "kms", Owner: v.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every table test below issues records for the "human" audience while the
// store's bootstrap audience is "client" (newReplicaKV/NewKVKeyStore calls
// throughout this file), so the always-present bootstrap key never becomes a
// candidate and each case's outcome depends only on the records under test.

func TestKVKeyStore_SignerSelectionRule(t *testing.T) {
	almostNow := time.Now().Add(-time.Minute)
	earlier := almostNow.Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	type rec struct {
		kid    string
		active bool
		from   time.Time
		to     *time.Time
		broken bool
	}
	cases := []struct {
		name    string
		recs    []rec
		wantKID string
		wantErr error
	}{
		{
			name: "latest validFrom wins",
			recs: []rec{
				{kid: testKID("aaa"), active: true, from: earlier},
				{kid: testKID("bbb"), active: true, from: almostNow},
			},
			wantKID: testKID("bbb"),
		},
		{
			name: "tie on validFrom, greater KID wins",
			recs: []rec{
				{kid: testKID("aaa"), active: true, from: almostNow},
				{kid: testKID("bbb"), active: true, from: almostNow},
			},
			wantKID: testKID("bbb"),
		},
		{
			name:    "inactive owned key does not sign",
			recs:    []rec{{kid: testKID("aaa"), active: false, from: almostNow}},
			wantErr: ErrKeyPairNotFound,
		},
		{
			name:    "future-window owned key does not sign",
			recs:    []rec{{kid: testKID("aaa"), active: true, from: future}},
			wantErr: ErrKeyPairNotFound,
		},
		{
			name:    "expired owned key does not sign",
			recs:    []rec{{kid: testKID("aaa"), active: true, from: past, to: &almostNow}},
			wantErr: ErrKeyPairNotFound,
		},
		{
			name: "a broken record that would win fails closed",
			recs: []rec{
				{kid: testKID("aaa"), active: true, from: earlier},
				{kid: testKID("bbb"), active: true, from: almostNow, broken: true},
			},
			wantErr: ErrKeyPairBroken,
		},
		{
			name: "a broken record that loses does not block signing",
			recs: []rec{
				{kid: testKID("aaa"), active: true, from: earlier, broken: true},
				{kid: testKID("bbb"), active: true, from: almostNow},
			},
			wantKID: testKID("bbb"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := replicaSystemCtx()
			kv := newReplicaKV(t)
			boot := loadFixtureKey(t)
			bootKID, err := DeriveKID(&boot.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			v, err := NewWrappedVault(boot, bootKID)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range tc.recs {
				var b []byte
				if r.broken {
					b = brokenRecord(t, v, r.kid, "human", r.from, r.to)
				} else {
					b = issuedRecordFull(t, v, r.kid, "human", r.active, r.from, r.to)
				}
				if err := kv.Put(ctx, signingKeysNamespace, r.kid, b); err != nil {
					t.Fatal(err)
				}
			}
			s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
			if err != nil {
				t.Fatal(err)
			}
			kp, _, err := s.Signer("human")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kp.KID != tc.wantKID {
				t.Fatalf("kid = %q, want %q", kp.KID, tc.wantKID)
			}
		})
	}
}

func TestKVKeyStore_VerificationRule(t *testing.T) {
	almostPast := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	laterFuture := time.Now().Add(2 * time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name      string
		bootstrap bool
		active    bool
		from      time.Time
		to        *time.Time
		broken    bool
		deleted   bool
		wantOK    bool
	}{
		{name: "owned active in window verifies", active: true, from: almostPast, wantOK: true},
		{name: "owned inactive does not verify", active: false, from: almostPast, wantOK: false},
		{name: "owned future window does not verify", active: true, from: future, wantOK: false},
		{name: "owned expired does not verify", active: true, from: past, to: &almostPast, wantOK: false},
		{name: "broken record never verifies", active: true, from: almostPast, broken: true, wantOK: false},
		{name: "owned inactive with future validTo verifies (grace)", active: false, from: almostPast, to: &future, wantOK: true},
		{name: "owned inactive with past validTo does not verify", active: false, from: past, to: &almostPast, wantOK: false},
		{name: "owned inactive with future validFrom does not verify (window)", active: false, from: future, to: &laterFuture, wantOK: false},
		{name: "bootstrap state inactive with no validTo does not verify", bootstrap: true, active: false, from: time.Time{}, wantOK: false},
		{name: "bootstrap state inactive with future validTo verifies", bootstrap: true, active: false, from: time.Time{}, to: &future, wantOK: true},
		{name: "bootstrap state deleted with future validTo does not verify", bootstrap: true, active: false, from: time.Time{}, to: &future, deleted: true, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := replicaSystemCtx()
			kv := newReplicaKV(t)
			boot := loadFixtureKey(t)
			bootKID, err := DeriveKID(&boot.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			v, err := NewWrappedVault(boot, bootKID)
			if err != nil {
				t.Fatal(err)
			}
			kid := testKID("kid1")
			switch {
			case tc.bootstrap:
				kid = bootKID
				b, err := encodeSigningRecord(signingRecord{
					Kind: recordKindBootstrap, KID: bootKID, Active: tc.active,
					ValidFrom: fmtTime(tc.from), ValidTo: fmtTimePtr(tc.to), Deleted: tc.deleted,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := kv.Put(ctx, signingKeysNamespace, bootKID, b); err != nil {
					t.Fatal(err)
				}
			case tc.broken:
				if err := kv.Put(ctx, signingKeysNamespace, kid, brokenRecord(t, v, kid, "human", tc.from, tc.to)); err != nil {
					t.Fatal(err)
				}
			default:
				if err := kv.Put(ctx, signingKeysNamespace, kid, issuedRecordFull(t, v, kid, "human", tc.active, tc.from, tc.to)); err != nil {
					t.Fatal(err)
				}
			}
			s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.VerificationKey(kid)
			ok := err == nil
			if ok != tc.wantOK {
				t.Fatalf("VerificationKey ok = %v, want %v (err=%v)", ok, tc.wantOK, err)
			}
			if !tc.wantOK && !errors.Is(err, ErrKeyPairNotFound) {
				t.Fatalf("err = %v, want ErrKeyPairNotFound", err)
			}
		})
	}
}

func TestKVKeyStore_PublishedIncludesGraceExcludesExpiredSorted(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, bootKID)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	// Invalidated but still inside its grace window: must still be published
	// so a verifier holding a token it already signed can still check it.
	if err := kv.Put(ctx, signingKeysNamespace, testKID("zzz-grace"), issuedRecordFull(t, v, testKID("zzz-grace"), "human", false, past, &future)); err != nil {
		t.Fatal(err)
	}
	// Window fully ended: excluded.
	if err := kv.Put(ctx, signingKeysNamespace, testKID("aaa-expired"), issuedRecordFull(t, v, testKID("aaa-expired"), "human", true, past, &past)); err != nil {
		t.Fatal(err)
	}
	// Active, no window end: included.
	if err := kv.Put(ctx, signingKeysNamespace, testKID("mmm-active"), issuedRecordFull(t, v, testKID("mmm-active"), "human", true, past, nil)); err != nil {
		t.Fatal(err)
	}
	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := s.Published()
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	present := map[string]bool{}
	for _, p := range pub {
		kids = append(kids, p.KID)
		present[p.KID] = true
	}
	if !sort.StringsAreSorted(kids) {
		t.Fatalf("Published not sorted by KID: %v", kids)
	}
	if !present[testKID("zzz-grace")] {
		t.Fatal("expected an invalidated-in-grace key to be published")
	}
	if present[testKID("aaa-expired")] {
		t.Fatal("expected an expired key to be excluded from Published")
	}
	if !present[testKID("mmm-active")] {
		t.Fatal("expected an active key with no window end to be published")
	}
}

// Published must never list a record Verifies(now) refuses. An inactive
// record with no validTo is not reachable through the admin API (Invalidate
// always sets validTo via graceExpiry), but a hand-written or foreign-node
// record could still take this shape, and JWKS must not publish a key that
// verifies nothing.
func TestKVKeyStore_PublishedExcludesInactiveNoValidTo(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, bootKID)
	if err != nil {
		t.Fatal(err)
	}
	almostPast := time.Now().Add(-time.Minute)
	if err := kv.Put(ctx, signingKeysNamespace, testKID("dead"), issuedRecordFull(t, v, testKID("dead"), "human", false, almostPast, nil)); err != nil {
		t.Fatal(err)
	}
	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := s.Published()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pub {
		if p.KID == testKID("dead") {
			t.Fatalf("inactive record with no validTo must not be published: %+v", p)
		}
	}
}

func TestKVKeyStore_DeletedBootstrapRefusesVerifyAndSign(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	rec := signingRecord{Kind: recordKindBootstrap, KID: bootKID, Active: true, ValidFrom: fmtTime(time.Time{}), Deleted: true}
	b, err := encodeSigningRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, bootKID, b); err != nil {
		t.Fatal(err)
	}
	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerificationKey(bootKID); !errors.Is(err, ErrKeyPairNotFound) {
		t.Fatalf("verify: err = %v, want ErrKeyPairNotFound", err)
	}
	if _, _, err := s.Signer("client"); !errors.Is(err, ErrKeyPairNotFound) {
		t.Fatalf("sign: err = %v, want ErrKeyPairNotFound", err)
	}
}

// The retired-set WARN logs once per change to the set, not on every re-read.
func TestKVKeyStore_RetiredWarnLoggedOncePerChange(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	otherBoot, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKID, err := DeriveKID(&otherBoot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	otherVault, err := NewWrappedVault(otherBoot, otherKID)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, testKID("retired-1"), issuedRecord(t, otherVault, testKID("retired-1"), "human")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "retired on this node"); n != 1 {
		t.Fatalf("expected exactly one retired WARN after load, got %d; log: %s", n, buf.String())
	}
	buf.Reset()
	if err := s.rep.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "retired on this node"); n != 0 {
		t.Fatalf("expected no retired WARN on an unchanged re-read, got %d; log: %s", n, buf.String())
	}
	if err := kv.Put(ctx, signingKeysNamespace, testKID("retired-2"), issuedRecord(t, otherVault, testKID("retired-2"), "human")); err != nil {
		t.Fatal(err)
	}
	if err := s.rep.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "retired on this node"); n != 1 {
		t.Fatalf("expected one retired WARN after the retired set changed, got %d; log: %s", n, buf.String())
	}
}

func TestKVKeyStore_LogRevokedBootstrapWhenOwnedPairsExist(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, bootKID)
	if err != nil {
		t.Fatal(err)
	}
	rec := signingRecord{Kind: recordKindBootstrap, KID: bootKID, Active: false, ValidFrom: fmtTime(time.Time{})}
	b, err := encodeSigningRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, bootKID, b); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, testKID("issued-1"), issuedRecord(t, v, testKID("issued-1"), "human")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	if _, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no longer signs") {
		t.Fatalf("expected the revoked-bootstrap INFO; log: %s", buf.String())
	}
}

func TestKVKeyStore_NoLogRevokedBootstrapWhenNoOwnedPairs(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	rec := signingRecord{Kind: recordKindBootstrap, KID: bootKID, Active: false, ValidFrom: fmtTime(time.Time{})}
	b, err := encodeSigningRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, bootKID, b); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	if _, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "no longer signs") {
		t.Fatalf("expected no revoked-bootstrap INFO with zero owned pairs; log: %s", buf.String())
	}
}

// ownedIssuedCount must not count a broken record whose reason is "unknown
// vault kind": that record was never sealed under this bootstrap key's wrap
// scheme, so replacing the PEM would not unseal it, and it must not inflate
// the count that gates the revoked-bootstrap warning.
func TestKVKeyStore_LogRevokedBootstrapExcludesUnknownVaultKind(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, bootKID)
	if err != nil {
		t.Fatal(err)
	}
	rec := signingRecord{Kind: recordKindBootstrap, KID: bootKID, Active: false, ValidFrom: fmtTime(time.Time{})}
	b, err := encodeSigningRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, bootKID, b); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, "unknown-kind", unknownVaultKindRecord(t, v, "unknown-kind", "human")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	if _, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "no longer signs") {
		t.Fatalf("expected no revoked-bootstrap INFO when the only issued record is unknown-vault-kind; log: %s", buf.String())
	}
}

// With no issued key pair of the audience active and in its window, the
// signing key from configuration signs: it is an active key pair of that
// audience with a zero validFrom.
func TestKVKeyStore_SigningKeySignsWhenNoIssuedPairActive(t *testing.T) {
	boot := loadFixtureKey(t)
	s := newTestKeyStore(t, boot)
	bootKID, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := s.Issue(replicaSystemCtx(), IssueRequest{Audience: "client", ValidFrom: time.Now(), ValidTo: time.Now().Add(time.Hour), Invalidate: true})
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := s.Current("client"); err != nil || cur.KID != kp.KID {
		t.Fatalf("current = %v %v, want the issued pair", cur, err)
	}
	if err := s.Invalidate(replicaSystemCtx(), kp.KID, 0); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Current("client")
	if err != nil || cur.KID != bootKID {
		t.Fatalf("current = %v %v, want the signing key", cur, err)
	}
	signerKP, sg, err := s.Signer("client")
	if err != nil || sg == nil {
		t.Fatalf("no signer: %v", err)
	}
	if signerKP.KID != bootKID {
		t.Fatalf("signer = %s, want the signing key %s", signerKP.KID, bootKID)
	}
}
