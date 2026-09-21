package dispatch

import (
	"container/list"
	"errors"
	"net/http"
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

// A request refused for capacity leaves no entry behind, so without a
// watermark the very same envelope is accepted once the cache has room: an
// attacker on the path between nodes who captured it re-delivers it, the node
// runs it, and a processor that is not repeat-safe runs a second time — the
// owner having been told, under seal, that nothing was handed over.
//
// The refusal therefore raises a "refuse at or before" watermark to the refused
// request's own timestamp. The replay is refused identically for as long as it
// could still verify, and a genuine request caught by it costs no try.
func TestNonceCache_ARefusalForCapacityIsNotUndoneByRoom(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	c := newNonceCache(60*time.Second, 1, func() time.Time { return now })

	recorded := []byte("aaaaaaaaaaaa")
	refused := []byte("bbbbbbbbbbbb")
	if got := c.checkAndRecord(recorded, base); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh", got)
	}
	if got := c.checkAndRecord(refused, base); got != nonceCacheFull {
		t.Fatalf("verdict = %d, want nonceCacheFull", got)
	}

	// The cache empties: the one entry ages out.
	now = base.Add(90 * time.Second)
	if got := c.checkAndRecord(refused, base); got != nonceWatermarked {
		t.Fatalf("verdict = %d, want nonceWatermarked — the refused envelope is replayable again", got)
	}

	// A request stamped after the watermark is unaffected: the node is working
	// normally again.
	later := []byte("cccccccccccc")
	if got := c.checkAndRecord(later, base.Add(time.Second)); got != nonceFresh {
		t.Fatalf("verdict = %d, want nonceFresh for a request stamped after the refusal", got)
	}
}

// The watermark only ever rises: a refusal of an older request does not lower
// it, which would re-open the envelopes a later refusal closed.
func TestNonceCache_TheWatermarkOnlyRises(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	c := newNonceCache(60*time.Second, 1, func() time.Time { return now })

	c.checkAndRecord([]byte("aaaaaaaaaaaa"), base)
	if got := c.checkAndRecord([]byte("bbbbbbbbbbbb"), base.Add(10*time.Second)); got != nonceCacheFull {
		t.Fatalf("verdict = %d, want nonceCacheFull", got)
	}
	// An older request is refused too, and must not pull the watermark back.
	if got := c.checkAndRecord([]byte("cccccccccccc"), base.Add(2*time.Second)); got != nonceCacheFull {
		t.Fatalf("verdict = %d, want nonceCacheFull", got)
	}
	now = base.Add(90 * time.Second)
	if got := c.checkAndRecord([]byte("dddddddddddd"), base.Add(5*time.Second)); got != nonceWatermarked {
		t.Fatalf("verdict = %d, want nonceWatermarked — the watermark was pulled back", got)
	}
}

// The two capacity-side refusals are one class to a caller and two lines to an
// operator: a saturated cache is a flood, the watermark is its aftermath, and
// the answer to the owner is the same for both.
func TestVerify_TellsAFullCacheFromAWatermarkRefusal(t *testing.T) {
	a := newAEAD(t)
	a.nonces = newNonceCache(time.Minute, 1, time.Now)

	first, _, _ := newBoundRequest(t, a, handOverPath, []byte(`{}`))
	if _, _, _, err := a.Verify(first); err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	// The cache is at capacity: refused for capacity, and the watermark rises.
	full, _, _ := newBoundRequest(t, a, handOverPath, []byte(`{}`))
	_, _, binding, err := a.Verify(full)
	if !errors.Is(err, ErrReplayCacheFull) {
		t.Fatalf("err = %v, want the replay-cache class", err)
	}
	if errors.Is(err, ErrReplayWatermarked) {
		t.Errorf("a refusal for capacity was reported as a watermark refusal: %v", err)
	}
	if _, sealErr := a.SealResponse(http.Header{}, binding, []byte(`{}`)); sealErr != nil {
		t.Errorf("the binding that came with the refusal does not seal: %v", sealErr)
	}

	// Room again, but the watermark stands: same class, different reason, same
	// sealed answer.
	a.nonces.entries = map[string]*list.Element{}
	a.nonces.order = list.New()
	marked, _, _ := newBoundRequest(t, a, handOverPath, []byte(`{}`))
	_, _, binding, err = a.Verify(marked)
	if !errors.Is(err, ErrReplayWatermarked) || !errors.Is(err, ErrReplayCacheFull) {
		t.Fatalf("err = %v, want a watermark refusal inside the replay-cache class", err)
	}
	if _, sealErr := a.SealResponse(http.Header{}, binding, []byte(`{}`)); sealErr != nil {
		t.Errorf("the binding that came with the refusal does not seal: %v", sealErr)
	}
}
