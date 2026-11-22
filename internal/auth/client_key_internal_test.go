package auth

import (
	"crypto/sha256"
	"testing"
	"time"
)

func TestClientBuckets_SameIDInTwoTenantsHaveOwnBuckets(t *testing.T) {
	b := newClientBuckets(1)
	now := time.Now()
	if ok, _ := b.allow(clientKey{"a", "backend"}, now); !ok {
		t.Fatal("first request of a refused")
	}
	if ok, _ := b.allow(clientKey{"a", "backend"}, now); ok {
		t.Fatal("a's bucket not limited")
	}
	if ok, _ := b.allow(clientKey{"b", "backend"}, now); !ok {
		t.Fatal("b throttled by a's bucket")
	}
}

func TestVerifiedSecretCache_SameIDInTwoTenantsHaveOwnEntries(t *testing.T) {
	c := newVerifiedSecretCache(10)
	sum := sha256.Sum256([]byte("s"))
	c.put(clientKey{"a", "backend"}, "hashA", sum)
	c.drop(clientKey{"b", "backend"})
	if !c.hit(clientKey{"a", "backend"}, "hashA", sum) {
		t.Fatal("b's drop evicted a's entry")
	}
	if c.hit(clientKey{"b", "backend"}, "hashA", sum) {
		t.Fatal("b hit a's entry")
	}
}
