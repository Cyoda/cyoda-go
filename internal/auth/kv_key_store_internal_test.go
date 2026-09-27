package auth

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf is a concurrency-safe slog sink: the ping's reconcile can log
// from its own goroutine while the test goroutine is still inspecting or
// about to inspect the buffer, and a plain bytes.Buffer is not safe for that.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// signerCached is a white-box accessor for the tests below: it reaches into
// the classifier's signer cache directly rather than adding a production
// accessor whose only caller would be a test.
func signerCached(s *KVKeyStore, kid string) bool {
	s.cls.signers.mu.Lock()
	defer s.cls.signers.mu.Unlock()
	_, ok := s.cls.signers.m[kid]
	return ok
}

// A record deleted from the store and re-read must not leave its opened
// signer cached forever: retain() runs after every swap/apply alongside
// logClassChanges so a deleted (or retired, or reclassified) owned record's
// signer leaves memory instead of accumulating. A second, still-owned record
// must keep its cached signer — retain must evict precisely, not empty the
// cache wholesale.
func TestKVKeyStore_RetainEvictsSignerOfDeletedRecord(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	kid, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, kid)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, "issued-1", issuedRecord(t, v, "issued-1", "client")); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, "issued-2", issuedRecord(t, v, "issued-2", "human")); err != nil {
		t.Fatal(err)
	}
	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if !signerCached(s, "issued-1") || !signerCached(s, "issued-2") {
		t.Fatal("expected both opened signers to be cached after the initial load")
	}
	if err := kv.Delete(ctx, signingKeysNamespace, "issued-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if signerCached(s, "issued-1") {
		t.Fatal("expected the deleted record's cached signer to have been evicted")
	}
	if !signerCached(s, "issued-2") {
		t.Fatal("expected the still-owned record's cached signer to remain — retain must not empty the whole cache")
	}
}

// Regression: a broadcaster that delivers a gossip message during Subscribe
// — before NewKVKeyStore has assigned s.rep — must not panic and must leave
// the store usable. Before the fix, the replica's afterChange callback took
// no arguments and KVKeyStore's implementation read s.rep.read(...) inside
// it; a ping arriving in that window ran the callback against a nil s.rep
// (a recovered panic logged as an ERROR, and an unsynchronised read/write of
// s.rep). The fix passes the replica's copy directly into the callback, so
// KVKeyStore's callback never touches s.rep at all.
//
// immediateBroadcaster blocks Subscribe for longer than a re-read takes, so
// the ping's triggered reconcile — and, pre-fix, its panic — has already run
// by the time NewKVKeyStore returns: this is not a maybe.
func TestKVKeyStore_GossipDuringConstructionDoesNotPanic(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)

	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	defer slog.SetDefault(prev)

	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{
		Bootstrap: boot, BootstrapAudience: "client", Broadcaster: immediateBroadcaster{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Belt-and-braces margin beyond immediateBroadcaster's own 300ms block, in
	// case a slow CI host pushes the reconcile past it.
	deadline := time.Now().Add(2 * time.Second)
	for s.rep.ping.busy() {
		if time.Now().After(deadline) {
			t.Fatal("ping never settled")
		}
		time.Sleep(time.Millisecond)
	}
	if strings.Contains(buf.String(), "panic") {
		t.Fatalf("gossip during construction must not panic; log: %s", buf.String())
	}
	if _, _, err := s.Signer("client"); err != nil {
		t.Fatalf("store must still work after construction-time gossip: %v", err)
	}
}
