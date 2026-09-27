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
				{kid: "aaa", active: true, from: earlier},
				{kid: "bbb", active: true, from: almostNow},
			},
			wantKID: "bbb",
		},
		{
			name: "tie on validFrom, greater KID wins",
			recs: []rec{
				{kid: "aaa", active: true, from: almostNow},
				{kid: "bbb", active: true, from: almostNow},
			},
			wantKID: "bbb",
		},
		{
			name:    "inactive owned key does not sign",
			recs:    []rec{{kid: "aaa", active: false, from: almostNow}},
			wantErr: ErrKeyPairNotFound,
		},
		{
			name:    "future-window owned key does not sign",
			recs:    []rec{{kid: "aaa", active: true, from: future}},
			wantErr: ErrKeyPairNotFound,
		},
		{
			name:    "expired owned key does not sign",
			recs:    []rec{{kid: "aaa", active: true, from: past, to: &almostNow}},
			wantErr: ErrKeyPairNotFound,
		},
		{
			name: "a broken record that would win fails closed",
			recs: []rec{
				{kid: "aaa", active: true, from: earlier},
				{kid: "bbb", active: true, from: almostNow, broken: true},
			},
			wantErr: ErrKeyPairBroken,
		},
		{
			name: "a broken record that loses does not block signing",
			recs: []rec{
				{kid: "aaa", active: true, from: earlier, broken: true},
				{kid: "bbb", active: true, from: almostNow},
			},
			wantKID: "bbb",
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
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name   string
		active bool
		from   time.Time
		to     *time.Time
		broken bool
		wantOK bool
	}{
		{name: "owned active in window verifies", active: true, from: almostPast, wantOK: true},
		{name: "owned inactive does not verify", active: false, from: almostPast, wantOK: false},
		{name: "owned future window does not verify", active: true, from: future, wantOK: false},
		{name: "owned expired does not verify", active: true, from: past, to: &almostPast, wantOK: false},
		{name: "broken record never verifies", active: true, from: almostPast, broken: true, wantOK: false},
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
			var b []byte
			if tc.broken {
				b = brokenRecord(t, v, "kid1", "human", tc.from, tc.to)
			} else {
				b = issuedRecordFull(t, v, "kid1", "human", tc.active, tc.from, tc.to)
			}
			if err := kv.Put(ctx, signingKeysNamespace, "kid1", b); err != nil {
				t.Fatal(err)
			}
			s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.VerificationKey("kid1")
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
	if err := kv.Put(ctx, signingKeysNamespace, "zzz-grace", issuedRecordFull(t, v, "zzz-grace", "human", false, past, &future)); err != nil {
		t.Fatal(err)
	}
	// Window fully ended: excluded.
	if err := kv.Put(ctx, signingKeysNamespace, "aaa-expired", issuedRecordFull(t, v, "aaa-expired", "human", true, past, &past)); err != nil {
		t.Fatal(err)
	}
	// Active, no window end: included.
	if err := kv.Put(ctx, signingKeysNamespace, "mmm-active", issuedRecordFull(t, v, "mmm-active", "human", true, past, nil)); err != nil {
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
	if !present["zzz-grace"] {
		t.Fatal("expected an invalidated-in-grace key to be published")
	}
	if present["aaa-expired"] {
		t.Fatal("expected an expired key to be excluded from Published")
	}
	if !present["mmm-active"] {
		t.Fatal("expected an active key with no window end to be published")
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
	if err := kv.Put(ctx, signingKeysNamespace, "retired-1", issuedRecord(t, otherVault, "retired-1", "human")); err != nil {
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
	if err := kv.Put(ctx, signingKeysNamespace, "retired-2", issuedRecord(t, otherVault, "retired-2", "human")); err != nil {
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
	if err := kv.Put(ctx, signingKeysNamespace, "issued-1", issuedRecord(t, v, "issued-1", "human")); err != nil {
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
