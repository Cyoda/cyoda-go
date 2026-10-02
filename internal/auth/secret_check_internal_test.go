package auth

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// The cache holds at most its bound: an entry it drops costs that client one
// bcrypt on its next request, never a wrong answer.
func TestVerifiedSecretCache_Bounded(t *testing.T) {
	c := newVerifiedSecretCache(16)
	for i := 0; i < 100; i++ {
		c.put(fmt.Sprintf("C%d", i), "h", sha256.Sum256([]byte("s")))
	}
	n := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.entries)
	}()
	if n > 16 {
		t.Fatalf("%d entries, bound 16", n)
	}
	if !c.hit("C99", "h", sha256.Sum256([]byte("s"))) {
		t.Fatal("the entry just stored is not a hit")
	}
}

func TestVerifiedSecretCache_HitNeedsHashAndSecret(t *testing.T) {
	c := newVerifiedSecretCache(16)
	sum := sha256.Sum256([]byte("s"))
	c.put("C1", "h1", sum)
	if !c.hit("C1", "h1", sum) {
		t.Fatal("same hash and secret: not a hit")
	}
	if c.hit("C1", "h2", sum) {
		t.Fatal("another stored hash: a hit")
	}
	if c.hit("C1", "h1", sha256.Sum256([]byte("t"))) {
		t.Fatal("another secret: a hit")
	}
	c.drop("C1")
	if c.hit("C1", "h1", sum) {
		t.Fatal("a dropped entry: a hit")
	}
}
