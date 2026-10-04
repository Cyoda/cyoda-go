package auth

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The cache holds at most its bound: an entry it drops costs that client one
// bcrypt on its next request, never a wrong answer.
func TestVerifiedSecretCache_Bounded(t *testing.T) {
	c := newVerifiedSecretCache(16)
	for i := 0; i < 100; i++ {
		c.put(clientKey{"t", fmt.Sprintf("C%d", i)}, "h", sha256.Sum256([]byte("s")))
	}
	n := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.entries)
	}()
	if n > 16 {
		t.Fatalf("%d entries, bound 16", n)
	}
	if !c.hit(clientKey{"t", "C99"}, "h", sha256.Sum256([]byte("s"))) {
		t.Fatal("the entry just stored is not a hit")
	}
}

func TestVerifiedSecretCache_HitNeedsHashAndSecret(t *testing.T) {
	c := newVerifiedSecretCache(16)
	sum := sha256.Sum256([]byte("s"))
	c.put(clientKey{"t", "C1"}, "h1", sum)
	if !c.hit(clientKey{"t", "C1"}, "h1", sum) {
		t.Fatal("same hash and secret: not a hit")
	}
	if c.hit(clientKey{"t", "C1"}, "h2", sum) {
		t.Fatal("another stored hash: a hit")
	}
	if c.hit(clientKey{"t", "C1"}, "h1", sha256.Sum256([]byte("t"))) {
		t.Fatal("another secret: a hit")
	}
	c.drop(clientKey{"t", "C1"})
	if c.hit(clientKey{"t", "C1"}, "h1", sum) {
		t.Fatal("a dropped entry: a hit")
	}
}

// holdSlots takes all n secret-check slots of s and returns the function
// that gives them back.
func holdSlots(t *testing.T, s *KVM2MClientStore, n int) (release func()) {
	t.Helper()
	if !s.slots.sem.TryAcquire(int64(n)) {
		t.Fatal("slots already taken")
	}
	return func() { s.slots.sem.Release(int64(n)) }
}

// A cached secret is accepted without a slot; anything else needs one. With
// the only slot held, the right secret still succeeds, a wrong secret is
// refused as busy, and that refusal leaves the cached entry in place.
func TestSecretCache_HitSkipsTheSlot(t *testing.T) {
	s := NewKVM2MClientStore(newReplicaKV(t), 0, SecretCheckLimit{Slots: 1, Wait: time.Millisecond})
	ctx := replicaSystemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); err != nil { // warms the cache
		t.Fatal(err)
	}
	defer holdSlots(t, s, 1)()
	if _, err := s.Authenticate(ctx, "C1", sec); err != nil {
		t.Fatalf("right secret with no free slot: %v, want a cache hit", err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec+"x"); !errors.Is(err, ErrSecretCheckBusy) {
		t.Fatalf("wrong secret with no free slot: %v, want ErrSecretCheckBusy", err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); err != nil {
		t.Fatalf("right secret after a wrong one: %v, want a cache hit", err)
	}
}

// A wrong secret that reaches bcrypt does not evict the client's cached
// entry: anyone can present a client id, so a refusal must not cost the
// client its warm entry.
func TestSecretCache_WrongSecretKeepsTheEntry(t *testing.T) {
	s := NewKVM2MClientStore(newReplicaKV(t), 0, SecretCheckLimit{Slots: 1, Wait: time.Millisecond})
	ctx := replicaSystemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec+"x"); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("wrong secret: %v", err)
	}
	defer holdSlots(t, s, 1)()
	if _, err := s.Authenticate(ctx, "C1", sec); err != nil {
		t.Fatalf("right secret after a refused one, no free slot: %v, want a cache hit", err)
	}
}

// Hashing a new secret is bcrypt work too: Create and ResetSecret take a
// slot, and with none free they are ErrSecretCheckBusy and write nothing.
func TestSecretCheck_CreateAndResetTakeASlot(t *testing.T) {
	s := NewKVM2MClientStore(newReplicaKV(t), 0, SecretCheckLimit{Slots: 1, Wait: time.Millisecond})
	ctx := replicaSystemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	release := holdSlots(t, s, 1)
	_, createErr := s.Create(ctx, "acme", "C2", "C2", []string{"ROLE_M2M"}, false)
	_, _, resetErr := s.ResetSecret(ctx, "acme", "C1")
	release()
	if !errors.Is(createErr, ErrSecretCheckBusy) {
		t.Errorf("Create with no free slot: %v, want ErrSecretCheckBusy", createErr)
	}
	if !errors.Is(resetErr, ErrSecretCheckBusy) {
		t.Errorf("ResetSecret with no free slot: %v, want ErrSecretCheckBusy", resetErr)
	}
	if _, err := s.Lookup(ctx, "C2"); !errors.Is(err, ErrM2MClientNotFound) {
		t.Errorf("a refused Create wrote a client: %v", err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); err != nil {
		t.Errorf("a refused ResetSecret changed the secret: %v", err)
	}
}
