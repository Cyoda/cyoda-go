// Package modelcache caches LOCKED model descriptors per node, in front of the
// plugin's model store. An UNLOCKED descriptor is never cached: its schema can
// still change.
//
// A LOCKED schema only grows (by ExtendSchema), so a stale entry can only miss
// fields, never claim one. The cache keeps that window short:
//
//   - every invalidation — a local model mutation or extension, or a
//     "model.invalidate" broadcast from a peer — evicts the entry and bumps the
//     key's generation, and a fill that read storage before the bump is dropped;
//   - the jittered lease expires an entry whose broadcast was lost.
//
// Misses at one generation share one storage read; identical extensions on
// one node share one storage call. See
// docs/superpowers/specs/2026-11-12-710-schema-extensions-design.md §4.5, §4.7.
package modelcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// Clock abstracts time.Now so tests can drive expiry deterministically.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() } // realtime: model cache lease

// CachingModelStore is one node's model cache state: the entries, their
// invalidation generations and the shared calls. It reads storage only through
// the per-request store a CachingStoreFactory hands out. Zero value not safe —
// use New.
type CachingModelStore struct {
	broadcaster spi.ClusterBroadcaster
	clock       Clock
	lease       time.Duration

	mu      sync.RWMutex
	entries map[cacheKey]cacheEntry
	// generations counts the invalidations each key has seen. It outlives the
	// entry — evict leaves it — so a fill that read storage before an
	// invalidation can always tell. One counter per (tenant, model) ever
	// invalidated on this node.
	generations map[cacheKey]uint64
	jitterRand  *rand.Rand // guarded by mu

	// reads coalesces storage reads (misses and refreshes) per (key,
	// generation); extends coalesces identical extensions per (key, delta).
	reads   singleflight.Group
	extends singleflight.Group
}

type cacheKey struct {
	tenant string
	ref    spi.ModelRef
}

type cacheEntry struct {
	desc      *spi.ModelDescriptor
	expiresAt time.Time

	// node is desc.Schema parsed, derived once when the entry is built and
	// discarded with it. Deriving it is the dominant cost of evaluating a
	// workflow criterion — the raw bytes were already cached here, the parse
	// was not, so every evaluation rebuilt the whole field tree. Holding it on
	// the entry means it inherits this entry's eviction and lease, so there is
	// exactly one thing to invalidate and no way for a parse to outlive the
	// bytes it came from.
	//
	// nodeErr is the parse failure, retained so a broken schema reports the
	// same error every time without re-parsing to rediscover it. A descriptor
	// with no schema bound yields (nil, nil) — "no type constraints", not an
	// error, matching the callers this replaces.
	node    *schema.ModelNode
	nodeErr error
}

// parseSchemaNode derives the parsed schema for desc. The (nil, nil) result for
// an absent or unbound schema, and the error wording, match what the search
// package's fieldsFromDescriptor produced when it did this per call.
func parseSchemaNode(desc *spi.ModelDescriptor) (*schema.ModelNode, error) {
	if desc == nil || len(desc.Schema) == 0 {
		return nil, nil
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal model schema: %w", err)
	}
	return node, nil
}

// New constructs a CachingModelStore.
//
// broadcaster may be nil for single-node deployments — invalidation
// then only fires locally.
//
// clock may be nil to use the wall clock.
//
// lease must be > 0; actual expiry is jittered ±10% to prevent the
// cross-node herd that a uniform lease would produce.
func New(broadcaster spi.ClusterBroadcaster, clk Clock, lease time.Duration) *CachingModelStore {
	if clk == nil {
		clk = realClock{}
	}
	c := &CachingModelStore{
		broadcaster: broadcaster,
		clock:       clk,
		lease:       lease,
		jitterRand:  rand.New(rand.NewPCG(uint64(clk.Now().UnixNano()), 0xC0DA6060)),
		entries:     make(map[cacheKey]cacheEntry),
		generations: make(map[cacheKey]uint64),
	}
	if broadcaster != nil {
		broadcaster.Subscribe(topicModelInvalidate, c.handleInvalidation)
	}
	return c
}

// ---- entries and generations ------------------------------------------

// lookupEntry returns the live entry for key, or ok=false when absent or past
// its lease.
func (c *CachingModelStore) lookupEntry(key cacheKey) (cacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok || c.clock.Now().After(e.expiresAt) {
		return cacheEntry{}, false
	}
	return e, true
}

// generation returns key's invalidation generation.
func (c *CachingModelStore) generation(key cacheKey) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generations[key]
}

// bump advances key's invalidation generation. Every fill that recorded an
// earlier generation is dropped from then on.
func (c *CachingModelStore) bump(key cacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generations[key]++
}

// storeIfGeneration caches desc under key only if key's generation is still
// gen, the value recorded before the read that produced desc. Otherwise an
// invalidation landed during the read, desc may predate it, and nothing is
// stored.
func (c *CachingModelStore) storeIfGeneration(key cacheKey, gen uint64, desc *spi.ModelDescriptor) {
	// Derived outside the lock: parsing is the expensive part and depends only
	// on desc, so holding the write lock across it would serialise every reader
	// behind an unrelated model's parse.
	node, nodeErr := parseSchemaNode(desc)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generations[key] != gen {
		return
	}
	c.entries[key] = cacheEntry{
		desc:      desc,
		expiresAt: c.clock.Now().Add(c.jitteredLeaseLocked()),
		node:      node,
		nodeErr:   nodeErr,
	}
}

func (c *CachingModelStore) evict(key cacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// dropLocal bumps key's generation, then evicts its entry. In that order a
// fill racing the two either fails the generation check or is evicted.
func (c *CachingModelStore) dropLocal(key cacheKey) {
	c.bump(key)
	c.evict(key)
}

// invalidate drops key on this node and tells the peers to do the same.
func (c *CachingModelStore) invalidate(key cacheKey) {
	c.dropLocal(key)
	if c.broadcaster == nil {
		return
	}
	// EncodeInvalidation marshals three strings; it cannot fail.
	if payload, err := EncodeInvalidation(key.tenant, key.ref); err == nil {
		c.broadcaster.Broadcast(topicModelInvalidate, payload)
	}
}

func (c *CachingModelStore) handleInvalidation(payload []byte) {
	tenant, ref, ok := DecodeInvalidation(payload)
	if !ok {
		return
	}
	c.dropLocal(cacheKey{tenant: tenant, ref: ref})
}

// jitteredLeaseLocked returns lease ± 10 %. MUST be called with c.mu
// held (exclusive): jitterRand is not concurrent-safe.
func (c *CachingModelStore) jitteredLeaseLocked() time.Duration {
	factor := 0.9 + 0.2*c.jitterRand.Float64()
	return time.Duration(float64(c.lease) * factor)
}

// ---- shared calls -------------------------------------------------------

// readThrough reads key from storage through the (key, generation) flight and
// caches a LOCKED result under the fill guard. It does not consult the cached
// entry: Get does that first, RefreshAndGet skips it. Neither evicts nor bumps.
func (c *CachingModelStore) readThrough(ctx context.Context, key cacheKey, inner spi.ModelStore) (*spi.ModelDescriptor, error) {
	for {
		gen := c.generation(key)
		ch := c.reads.DoChan(flightKey(key, gen), recovering(func() (any, error) {
			desc, err := inner.Get(ctx, key.ref)
			if err != nil {
				return nil, sharedCallError(ctx, err)
			}
			if desc != nil && desc.State == spi.ModelLocked {
				c.storeIfGeneration(key, gen, desc)
			}
			return desc, nil
		}))
		v, retry, err := awaitShared(ctx, ch)
		if retry {
			continue
		}
		if err != nil {
			return nil, err
		}
		desc, _ := v.(*spi.ModelDescriptor)
		return desc, nil
	}
}

// extend runs inner.ExtendSchema once for every concurrent caller with the same
// tenant, model and delta, on the first caller's context. When the inner call
// ends, however it ended (a result, an error or a panic), the shared call
// itself invalidates key: the extension may have committed even when it
// failed, and the callers may all have left by then.
func (c *CachingModelStore) extend(ctx context.Context, key cacheKey, delta spi.SchemaDelta, inner spi.ModelStore) error {
	for {
		ch := c.extends.DoChan(extendKey(key, delta), recovering(func() (any, error) {
			defer c.invalidate(key)
			return nil, sharedCallError(ctx, inner.ExtendSchema(ctx, key.ref, delta))
		}))
		_, retry, err := awaitShared(ctx, ch)
		if !retry {
			return err
		}
	}
}

// leaderEndedError marks a shared call's failure that came after the context
// it ran on — its first caller's — had ended. Whatever form the backend gave
// the error (a context error, a driver or a classified error), the failure may
// be that caller's cancellation, not an answer for the others.
type leaderEndedError struct{ err error }

func (e *leaderEndedError) Error() string { return e.err.Error() }
func (e *leaderEndedError) Unwrap() error { return e.err }

// sharedCallError tags err when ctx, the context the shared call ran on, has
// ended. A nil err stays nil.
func sharedCallError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	return &leaderEndedError{err: err}
}

// sharedPanic carries a panic out of a shared call. DoChan runs the call on a
// goroutine of its own, where singleflight re-raises a panic beyond any
// recovery and the process dies; recovering it there and re-raising it in each
// waiting caller (awaitShared) puts it back on a request goroutine, whose door
// answers it as it answers any other panic.
type sharedPanic struct{ value any }

func (p *sharedPanic) Error() string { return fmt.Sprintf("panic in shared model call: %v", p.value) }

// recovering wraps a shared call's function so a panic in it becomes a
// sharedPanic result.
func recovering(fn func() (any, error)) func() (any, error) {
	return func() (v any, err error) {
		defer func() {
			if r := recover(); r != nil {
				v, err = nil, &sharedPanic{value: r}
			}
		}()
		return fn()
	}
}

// awaitShared waits for a shared call's result or for the caller's own ctx to
// end, whichever comes first, so no caller stays blocked on another caller's
// call. retry reports that the call failed after its first caller's context
// ended while this caller's is live: the caller starts a new call. A panic in
// the call is raised again here, in the caller.
func awaitShared(ctx context.Context, ch <-chan singleflight.Result) (v any, retry bool, err error) {
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case res := <-ch:
		if p, ok := res.Err.(*sharedPanic); ok {
			panic(p.value)
		}
		var ended *leaderEndedError
		if errors.As(res.Err, &ended) {
			if ctx.Err() == nil {
				return nil, true, nil
			}
			return nil, false, ended.err
		}
		return res.Val, false, res.Err
	}
}

// keyPrefix encodes key unambiguously: each field is length-prefixed, so what
// follows it (a generation, a delta) can never be read as part of it.
func keyPrefix(key cacheKey) string {
	return fmt.Sprintf("%d:%s%d:%s%d:%s", len(key.tenant), key.tenant,
		len(key.ref.EntityName), key.ref.EntityName,
		len(key.ref.ModelVersion), key.ref.ModelVersion)
}

func flightKey(key cacheKey, gen uint64) string {
	return keyPrefix(key) + strconv.FormatUint(gen, 10)
}

func extendKey(key cacheKey, delta spi.SchemaDelta) string {
	return keyPrefix(key) + string(delta)
}
