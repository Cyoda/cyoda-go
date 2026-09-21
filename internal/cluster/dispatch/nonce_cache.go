package dispatch

import (
	"container/list"
	"sync"
	"time"
)

// replayVerdict is what the replay cache made of a nonce. The refusals are told
// apart because they mean different things: a duplicate is a replay of a request
// that was already answered, and nothing about it may be confirmed under seal;
// the other two are this node failing closed on a request it authenticated,
// which it can say so — and an operator reading the log needs to know which,
// because a saturated cache is a flood and a watermark refusal is the aftermath
// of one.
type replayVerdict int

const (
	// nonceFresh: not seen within the TTL window, and now recorded.
	nonceFresh replayVerdict = iota
	// nonceDuplicate: seen within the window.
	nonceDuplicate
	// nonceCacheFull: at capacity after eviction. The nonce is refused although
	// it may well be a first sighting.
	nonceCacheFull
	// nonceWatermarked: room in the cache, but the request is stamped at or
	// before the last one capacity refused, so it may be that refused request
	// replayed. Refused the same way and with the same answer.
	nonceWatermarked
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
//
// A refusal for capacity records nothing, so the refused envelope would be
// accepted the moment the cache had room again — an attacker who captured it
// need only wait. refusedAtOrBefore closes that: it rises to the timestamp of
// every request capacity refuses, and any later request stamped at or before it
// whose nonce is not held is refused the same way. The refused envelope is
// therefore refused identically for as long as it could still verify at all,
// and a genuine request caught by the watermark costs no try. It needs no
// clearing: a request stamped at or before it is outside the skew window within
// one window of the refusal, and refused there anyway.
//
// Entries are kept in arrival order so that eviction stops at the first entry
// still inside the window. Scanning the whole map on every verification would
// mean a millisecond-scale sweep, under the one lock every verification shares,
// precisely when the node is busiest — and longest when the cache is full,
// which is the state this cache answers gracefully.
type nonceCache struct {
	ttl   time.Duration
	cap   int
	nowFn func() time.Time

	mu sync.Mutex
	// entries maps a nonce to its place in order, whose Value is a nonceEntry.
	entries map[string]*list.Element
	// order holds the entries oldest first.
	order *list.List
	// refusedAtOrBefore is the watermark described above; the zero time means
	// capacity has refused nothing.
	refusedAtOrBefore time.Time
}

// nonceEntry is one recorded nonce and the timestamp of the request that
// carried it — the request's own, not this node's clock, so an entry always
// outlives the window in which its request can verify.
type nonceEntry struct {
	key      string
	observed time.Time
}

func newNonceCache(ttl time.Duration, capacity int, nowFn func() time.Time) *nonceCache {
	return &nonceCache{
		ttl:     ttl,
		cap:     capacity,
		nowFn:   nowFn,
		entries: make(map[string]*list.Element, capacity),
		order:   list.New(),
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
		// Fail-closed: cache is full after eviction. The watermark rises so
		// that this envelope stays refused once there is room again.
		if observed.After(c.refusedAtOrBefore) {
			c.refusedAtOrBefore = observed
		}
		return nonceCacheFull
	}

	if !c.refusedAtOrBefore.IsZero() && !observed.After(c.refusedAtOrBefore) {
		// At or before a request capacity refused, and not held: either that
		// refused envelope replayed, or a request of the same moment. Refused
		// the same way, which uses no try.
		return nonceWatermarked
	}

	el := c.order.PushBack(nonceEntry{key: key, observed: observed})
	c.entries[key] = el
	return nonceFresh
}

// evictLocked removes the entries older than the TTL. They are in arrival
// order, so it stops at the first that is still inside the window rather than
// scanning the whole cache. Caller holds c.mu.
//
// Arrival order is not timestamp order — two nodes' clocks differ by up to the
// skew — so an entry a little out of order can outlive its TTL by up to that
// much. It is refused by the skew check long before, and refusing longer than
// necessary is the safe direction for a replay cache.
func (c *nonceCache) evictLocked() {
	cutoff := c.nowFn().Add(-c.ttl)
	for {
		front := c.order.Front()
		if front == nil {
			return
		}
		entry := front.Value.(nonceEntry)
		if !entry.observed.Before(cutoff) {
			return
		}
		c.order.Remove(front)
		delete(c.entries, entry.key)
	}
}
