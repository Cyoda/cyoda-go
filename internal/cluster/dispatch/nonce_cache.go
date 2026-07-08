package dispatch

import (
	"sync"
	"time"
)

// nonceCache is a bounded, TTL-expiring set used to reject replayed AEAD
// envelopes. Nonces are AES-GCM IVs — one per encryption — so a duplicate
// over the wire within the skew window is an attacker replaying a captured
// request.
//
// Fail-closed on capacity: when the cache is full, checkAndRecord reports
// *seen* for any new nonce. The caller surfaces this as a replay rejection.
// At realistic cluster rates (max ~10k dispatch/s) and a 60s TTL, a 100k
// ceiling leaves a 10x headroom over sustained load; an attacker flood fails
// closed rather than letting stale entries linger.
type nonceCache struct {
	ttl     time.Duration
	cap     int
	nowFn   func() time.Time
	mu      sync.Mutex
	entries map[string]time.Time
}

func newNonceCache(ttl time.Duration, capacity int, nowFn func() time.Time) *nonceCache {
	return &nonceCache{
		ttl:     ttl,
		cap:     capacity,
		nowFn:   nowFn,
		entries: make(map[string]time.Time, capacity),
	}
}

// checkAndRecord reports whether the nonce has been seen within the TTL
// window. If not, it records the nonce with the given observation time.
// Returns true only if the nonce is considered a replay (either a genuine
// duplicate, or a fail-closed capacity rejection).
func (c *nonceCache) checkAndRecord(nonce []byte, observed time.Time) bool {
	key := string(nonce)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.evictLocked()

	if _, exists := c.entries[key]; exists {
		return true
	}

	if len(c.entries) >= c.cap {
		// Fail-closed: cache is full after eviction; treat as replay.
		return true
	}

	c.entries[key] = observed
	return false
}

// evictLocked removes entries older than the TTL. Caller holds c.mu.
func (c *nonceCache) evictLocked() {
	cutoff := c.nowFn().Add(-c.ttl)
	for k, ts := range c.entries {
		if ts.Before(cutoff) {
			delete(c.entries, k)
		}
	}
}
