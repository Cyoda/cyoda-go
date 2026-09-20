package dispatch

import (
	"testing"
	"time"
)

func TestNonceCache_FirstObservationNotSeen(t *testing.T) {
	c := newNonceCache(60*time.Second, 100, time.Now)
	nonce := []byte("abcdefghijkl")
	if got := c.checkAndRecord(nonce, time.Now()); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh", got)
	}
}

// The two refusals are told apart: a duplicate is a replay of a request that
// was already answered, a full cache is this node failing closed. Only the
// second may be answered under seal.
func TestNonceCache_DuplicateIsDistinctFromAFullCache(t *testing.T) {
	c := newNonceCache(60*time.Second, 100, time.Now)
	nonce := []byte("abcdefghijkl")
	now := time.Now()
	if got := c.checkAndRecord(nonce, now); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh", got)
	}
	if got := c.checkAndRecord(nonce, now); got != nonceDuplicate {
		t.Fatalf("verdict = %d, want nonceDuplicate", got)
	}
}

// The duplicate check comes first: a replayed nonce is reported as a duplicate
// even when the cache is also full, so a saturated cache cannot be used to turn
// a replay into an authenticated "nothing happened".
func TestNonceCache_DuplicateWinsOverAFullCache(t *testing.T) {
	c := newNonceCache(60*time.Second, 1, time.Now)
	nonce := []byte("abcdefghijkl")
	now := time.Now()
	if got := c.checkAndRecord(nonce, now); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh", got)
	}
	if got := c.checkAndRecord(nonce, now); got != nonceDuplicate {
		t.Fatalf("verdict = %d, want nonceDuplicate even at capacity", got)
	}
}

func TestNonceCache_EvictsAfterTTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return base }
	c := newNonceCache(60*time.Second, 100, nowFn)
	nonce := []byte("abcdefghijkl")

	c.checkAndRecord(nonce, base)

	// Advance clock past TTL.
	base = base.Add(120 * time.Second)

	// After TTL has elapsed, the same nonce is no longer considered seen.
	if got := c.checkAndRecord(nonce, base); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh after the TTL expired", got)
	}
}

func TestNonceCache_BoundedSize_RejectsWhenFull(t *testing.T) {
	c := newNonceCache(60*time.Second, 3, time.Now)

	now := time.Now()
	n1 := []byte("aaaaaaaaaaaa")
	n2 := []byte("bbbbbbbbbbbb")
	n3 := []byte("cccccccccccc")
	n4 := []byte("dddddddddddd")

	for _, n := range [][]byte{n1, n2, n3} {
		if got := c.checkAndRecord(n, now); got != nonceFresh {
			t.Fatalf("verdict = %d for %q, want nonceFresh", got, n)
		}
	}

	// Cache is at capacity. A new nonce is refused (fail-closed), and the
	// refusal says which of the two it was: the caller answers it under seal.
	if got := c.checkAndRecord(n4, now); got != nonceCacheFull {
		t.Fatalf("verdict = %d, want nonceCacheFull", got)
	}
}

func TestNonceCache_CapacityRecoversAfterEviction(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return base }
	c := newNonceCache(60*time.Second, 2, nowFn)

	n1 := []byte("aaaaaaaaaaaa")
	n2 := []byte("bbbbbbbbbbbb")
	n3 := []byte("cccccccccccc")

	c.checkAndRecord(n1, base)
	c.checkAndRecord(n2, base)

	// Capacity full; n3 rejected.
	if got := c.checkAndRecord(n3, base); got != nonceCacheFull {
		t.Fatalf("verdict = %d, want nonceCacheFull", got)
	}

	// Advance past TTL — both n1 and n2 should evict.
	base = base.Add(120 * time.Second)

	// Now there's room; n3 is fresh.
	if got := c.checkAndRecord(n3, base); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh once eviction freed space", got)
	}
}

func TestNonceCache_DifferentNonceLengthsDistinct(t *testing.T) {
	c := newNonceCache(60*time.Second, 100, time.Now)
	now := time.Now()

	a := []byte("aaaaaaaaaaaa")
	b := []byte("aaaaaaaaaaab")
	if got := c.checkAndRecord(a, now); got != nonceFresh {
		t.Fatalf("verdict = %d for a, want nonceFresh", got)
	}
	if got := c.checkAndRecord(b, now); got != nonceFresh {
		t.Fatalf("verdict = %d for b, want nonceFresh: the cache is not byte-distinct", got)
	}
}
