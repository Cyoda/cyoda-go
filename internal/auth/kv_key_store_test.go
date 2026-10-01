package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func newBootstrap(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newKeyStore(t *testing.T, kv spi.KeyValueStore, boot *rsa.PrivateKey, aud string) *auth.KVKeyStore {
	t.Helper()
	s, err := auth.NewKVKeyStore(systemCtx(), kv, auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: aud})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// bootKID is the KID a store gives the bootstrap key boot.
func bootKID(t *testing.T, boot *rsa.PrivateKey) string {
	t.Helper()
	kid, err := auth.DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return kid
}

// newTestKeyStore is the signing-key store every package test uses: a
// KVKeyStore over a fresh in-memory KV store, boot signing for "client".
func newTestKeyStore(t *testing.T, boot *rsa.PrivateKey) *auth.KVKeyStore {
	t.Helper()
	return newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "client")
}

// issueWindow issues a key pair of aud with the window [from, to).
func issueWindow(t *testing.T, s *auth.KVKeyStore, aud string, from, to time.Time) *auth.KeyPair {
	t.Helper()
	kp, err := s.Issue(systemCtx(), auth.IssueRequest{Audience: aud, ValidFrom: from, ValidTo: to})
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

// ReconcileInterval is the re-read interval the store runs with: the
// configured one, or the default when none is configured.
func TestKVKeyStore_ReconcileInterval(t *testing.T) {
	boot := newBootstrap(t)
	for _, tc := range []struct {
		configured, want time.Duration
	}{
		{0, 60 * time.Second},
		{5 * time.Second, 5 * time.Second},
	} {
		s, err := auth.NewKVKeyStore(systemCtx(), mustNewMemoryKV(t, systemCtx()),
			auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client", ReconcileInterval: tc.configured})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.ReconcileInterval(); got != tc.want {
			t.Errorf("configured %v: ReconcileInterval() = %v, want %v", tc.configured, got, tc.want)
		}
	}
}

func TestKVKeyStore_BootstrapSignsByDefault(t *testing.T) {
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "client")
	kp, signer, err := s.Signer("client")
	if err != nil {
		t.Fatal(err)
	}
	if !kp.Bootstrap || kp.KID != bootKID(t, boot) || !signer.Public().(*rsa.PublicKey).Equal(&boot.PublicKey) {
		t.Fatalf("signer = %+v", kp)
	}
	if _, err := s.VerificationKey(bootKID(t, boot)); err != nil {
		t.Fatal(err)
	}
	pub, err := s.Published()
	if err != nil || len(pub) != 1 || pub[0].KID != bootKID(t, boot) {
		t.Fatalf("published = %v, %v", pub, err)
	}
	if _, _, err := s.Signer("human"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("human signer: err = %v, want ErrKeyPairNotFound", err)
	}
}

func TestDeriveKID_MatchesBootstrapKID(t *testing.T) {
	boot := newBootstrap(t)
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), boot, "client")
	kp, _, err := s.Signer("client")
	if err != nil {
		t.Fatal(err)
	}
	kid, err := auth.DeriveKID(&boot.PublicKey)
	if err != nil || kid != kp.KID || len(kid) != 32 {
		t.Fatalf("kid = %q, err = %v; the store signs as %q", kid, err, kp.KID)
	}
}

func TestKVKeyStore_UnknownKIDIsNotFound(t *testing.T) {
	s := newKeyStore(t, mustNewMemoryKV(t, systemCtx()), newBootstrap(t), "client")
	if _, err := s.VerificationKey("nope"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// An undecodable record at the bootstrap KID refuses the bootstrap key.
func TestKVKeyStore_UndecodableBootstrapStateRefusesBootstrap(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	boot := newBootstrap(t)
	kid, _ := auth.DeriveKID(&boot.PublicKey)
	_ = kv.Put(ctx, "signing-keys", kid, []byte("{"))
	s := newKeyStore(t, kv, boot, "client")
	if _, err := s.VerificationKey(kid); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("verification: err = %v", err)
	}
	if _, _, err := s.Signer("client"); !errors.Is(err, auth.ErrKeyPairBroken) {
		t.Fatalf("signer: err = %v, want ErrKeyPairBroken", err)
	}
}

// undecodableKID is a well-formed key id (32 lowercase hex) for records that
// do not decode: only a record at a key id is classified undecodable.
const undecodableKID = "0badc0de0badc0de0badc0de0badc0de"

func TestKVKeyStore_UndecodableRecordStopsSigning(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	_ = kv.Put(ctx, "signing-keys", undecodableKID, []byte("not json"))
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	s := newKeyStore(t, kv, newBootstrap(t), "client")
	if _, _, err := s.Signer("client"); !errors.Is(err, auth.ErrKeyPairBroken) {
		t.Fatalf("err = %v, want ErrKeyPairBroken", err)
	}
	if !strings.Contains(buf.String(), undecodableKID) || !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("expected an ERROR naming the record; log: %s", buf.String())
	}
}

func TestKVKeyStore_StaleFailsClosed(t *testing.T) {
	ctx := systemCtx()
	kv := &toggleListKV{KeyValueStore: mustNewMemoryKV(t, ctx)}
	boot := newBootstrap(t)
	s, err := auth.NewKVKeyStore(ctx, kv, auth.KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client", ReconcileInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	issued := issueWindow(t, s, "human", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	kv.fail.Store(true)
	loop, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(loop)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := s.Published()
		if errors.Is(err, auth.ErrStoreStale) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never became stale")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var su interface{ StorageUnavailable() bool }
	if !errors.As(auth.ErrStoreStale, &su) || !su.StorageUnavailable() {
		t.Fatal("ErrStoreStale must carry the storage-unavailable marker")
	}
	// While stale nothing verifies — a kid the last copy holds and a kid it
	// has never heard of both fail with ErrKeyPairNotFound.
	for _, kid := range []string{bootKID(t, boot), issued.KID, "0123456789abcdef0123456789abcdef"} {
		if _, err := s.VerificationKey(kid); !errors.Is(err, auth.ErrKeyPairNotFound) {
			t.Fatalf("verification of kid %q while stale: err = %v, want ErrKeyPairNotFound", kid, err)
		}
	}
	if _, _, err := s.Signer("client"); !errors.Is(err, auth.ErrStoreStale) {
		t.Fatalf("signer while stale: err = %v", err)
	}
}

// A record at a KV key that cannot be a key id (not 32 lowercase hex) is
// ignored: no key pair can have that id, so it cannot sign or be any
// bootstrap key's state, and it does not block signing. An ERROR names the
// key once, not on every re-read.
func TestKVKeyStore_NonKIDRecordIsIgnored(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	_ = kv.Put(ctx, "signing-keys", "junk", []byte("{"))
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	boot := newBootstrap(t)
	s := newKeyStore(t, kv, boot, "client")
	if kp, _, err := s.Signer("client"); err != nil || kp.KID != bootKID(t, boot) {
		t.Fatalf("signer = %v, err = %v; want the bootstrap key", kp, err)
	}
	if err := s.ReconcileForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 || !strings.Contains(buf.String(), "junk") {
		t.Fatalf("want one ERROR naming the key, got %d; log: %s", n, buf.String())
	}
	if strings.Contains(buf.String(), "{") {
		t.Fatalf("record value logged: %s", buf.String())
	}
	if err := s.Delete(ctx, "junk"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("delete: err = %v, want ErrKeyPairNotFound", err)
	}
	if _, err := s.VerificationKey("junk"); !errors.Is(err, auth.ErrKeyPairNotFound) {
		t.Fatalf("verification: err = %v", err)
	}
}

// Construction fails, and never serves an empty copy, when the initial List
// fails.
func TestKVKeyStore_ConstructionFailsWhenListFails(t *testing.T) {
	kv := &toggleListKV{KeyValueStore: mustNewMemoryKV(t, systemCtx())}
	kv.fail.Store(true)
	s, err := auth.NewKVKeyStore(systemCtx(), kv, auth.KVKeyStoreConfig{Bootstrap: newBootstrap(t), BootstrapAudience: "client"})
	if err == nil || s != nil {
		t.Fatalf("store = %v, err = %v; want a construction error", s, err)
	}
}
