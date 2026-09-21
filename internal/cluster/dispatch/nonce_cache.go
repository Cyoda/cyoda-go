package dispatch

import (
	"sync"
	"time"
)

// replayVerdict is what the replay cache made of a nonce. The two refusals are
// told apart because they mean different things to the caller: a duplicate is a
// replay of a request that was already answered, and nothing about it may be
// confirmed under seal; a full cache is this node failing closed on a request
// it authenticated, which it can say so.
type replayVerdict int

const (
	// nonceFresh: not seen within the TTL window, and now recorded.
	nonceFresh replayVerdict = iota
	// nonceDuplicate: seen within the window.
	nonceDuplicate
	// nonceCacheFull: at capacity after eviction. The nonce is refused
	// although it may well be a first sighting.
	nonceCacheFull
)

// nonceCache is a bounded, TTL-expiring set used to reject replayed AEAD
// envelopes. Nonces are AES-GCM IVs — one per encryption — so a duplicate
// over the wire within the skew window is an attacker replaying a captured
// request.
//
// Fail-closed on capacity: when the cache is full, checkAndRecord refuses any
// new nonce. The 100k ceiling over the 60s TTL window admits ~1,600 sustained
// inbound dispatches/s per node — far above realistic cross-node callout rates;
// an attacker flood fails closed rather than letting stale entries linger.
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

// checkAndRecord says what the cache makes of the nonce, recording it when it
// is fresh. The duplicate check comes before the capacity check, so a replayed
// nonce is reported as a duplicate even when the cache is also full: a
// saturated cache must not turn a replay into something answerable under seal.
func (c *nonceCache) checkAndRecord(nonce []byte, observed time.Time) replayVerdict {
	key := string(nonce)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.evictLocked()

	if _, exists := c.entries[key]; exists {
		return nonceDuplicate
	}

	if len(c.entries) >= c.cap {
		// Fail-closed: cache is full after eviction.
		return nonceCacheFull
	}

	c.entries[key] = observed
	return nonceFresh
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
