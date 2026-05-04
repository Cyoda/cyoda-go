// Package modelcache provides a CachingModelStore decorator that
// memoizes LOCKED model descriptors. Correctness rests on the
// catalog invariants (commutativity + validation-monotonicity from
// internal/domain/model/schema) plus the validator-path refresh-on-
// stale in internal/domain/entity/handler.go; gossip invalidation
// and the TTL lease are performance/hygiene layers. See
// docs/superpowers/specs/2026-04-20-model-schema-extensions-design.md §4.5.
package modelcache

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Clock abstracts time.Now so tests can drive expiry deterministically.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// CachingModelStore wraps an spi.ModelStore. Zero-value not safe — use New.
type CachingModelStore struct {
	inner       spi.ModelStore
	broadcaster spi.ClusterBroadcaster
	clock       Clock
	lease       time.Duration

	mu         sync.RWMutex
	entries    map[cacheKey]cacheEntry
	jitterRand *rand.Rand // guarded by mu

	flight singleflight.Group

	// localSubMu guards localSubs. Issue #174 — downstream caches
	// (e.g. the path-validation negative cache) register here to
	// receive an in-process notification on every invalidation,
	// regardless of whether a cluster broadcaster is wired up. On
	// single-node deployments this is the only invalidation channel
	// available; on multi-node it fires alongside the gossip path.
	localSubMu sync.RWMutex
	localSubs  []func(tenant string, ref spi.ModelRef)
}

type cacheKey struct {
	tenant string
	ref    spi.ModelRef
}

type cacheEntry struct {
	desc      *spi.ModelDescriptor
	expiresAt time.Time
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
func New(inner spi.ModelStore, broadcaster spi.ClusterBroadcaster, clk Clock, lease time.Duration) *CachingModelStore {
	if clk == nil {
		clk = realClock{}
	}
	c := &CachingModelStore{
		inner:       inner,
		broadcaster: broadcaster,
		clock:       clk,
		lease:       lease,
		jitterRand:  rand.New(rand.NewPCG(uint64(clk.Now().UnixNano()), 0xC0DA6060)),
		entries:     make(map[cacheKey]cacheEntry),
	}
	if broadcaster != nil {
		broadcaster.Subscribe(topicModelInvalidate, c.handleInvalidation)
	}
	return c
}

// EntryExpiresAt is test-only introspection for the jitter-band check.
// Returns the zero time if no entry is cached for ref in any tenant.
func (c *CachingModelStore) EntryExpiresAt(ref spi.ModelRef) time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for k, e := range c.entries {
		if k.ref == ref {
			return e.expiresAt
		}
	}
	return time.Time{}
}

// ---- spi.ModelStore pass-throughs with caching --------------------------

func (c *CachingModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	key := cacheKey{tenant: tenantOf(ctx), ref: ref}
	if d := c.lookup(key); d != nil {
		return d, nil
	}
	desc, err := c.inner.Get(ctx, ref)
	if err != nil || desc == nil {
		return desc, err
	}
	if desc.State != spi.ModelLocked {
		return desc, nil // UNLOCKED bypasses cache
	}
	c.store(key, desc)
	return desc, nil
}

// RefreshAndGet forces a cache miss, collapses concurrent callers via
// singleflight, then reads from the inner store and repopulates the
// cache. Used by the validator refresh-on-stale path.
func (c *CachingModelStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	key := cacheKey{tenant: tenantOf(ctx), ref: ref}
	c.evict(key)

	v, err, _ := c.flight.Do(flightKey(key), func() (any, error) {
		desc, err := c.inner.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		if desc != nil && desc.State == spi.ModelLocked {
			c.store(key, desc)
		}
		return desc, nil
	})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return v.(*spi.ModelDescriptor), nil
}

func (c *CachingModelStore) Save(ctx context.Context, desc *spi.ModelDescriptor) error {
	if err := c.inner.Save(ctx, desc); err != nil {
		return err
	}
	c.invalidate(ctx, desc.Ref)
	return nil
}

func (c *CachingModelStore) Lock(ctx context.Context, ref spi.ModelRef) error {
	if err := c.inner.Lock(ctx, ref); err != nil {
		return err
	}
	c.invalidate(ctx, ref)
	return nil
}

func (c *CachingModelStore) Unlock(ctx context.Context, ref spi.ModelRef) error {
	if err := c.inner.Unlock(ctx, ref); err != nil {
		return err
	}
	c.invalidate(ctx, ref)
	return nil
}

func (c *CachingModelStore) SetChangeLevel(ctx context.Context, ref spi.ModelRef, level spi.ChangeLevel) error {
	if err := c.inner.SetChangeLevel(ctx, ref, level); err != nil {
		return err
	}
	c.invalidate(ctx, ref)
	return nil
}

func (c *CachingModelStore) Delete(ctx context.Context, ref spi.ModelRef) error {
	if err := c.inner.Delete(ctx, ref); err != nil {
		return err
	}
	c.invalidate(ctx, ref)
	return nil
}

func (c *CachingModelStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if err := c.inner.ExtendSchema(ctx, ref, delta); err != nil {
		return err
	}
	c.invalidate(ctx, ref)
	return nil
}

func (c *CachingModelStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	return c.inner.GetAll(ctx)
}

func (c *CachingModelStore) IsLocked(ctx context.Context, ref spi.ModelRef) (bool, error) {
	return c.inner.IsLocked(ctx, ref)
}

// ---- internal helpers ---------------------------------------------------

func (c *CachingModelStore) lookup(key cacheKey) *spi.ModelDescriptor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	if c.clock.Now().After(e.expiresAt) {
		return nil
	}
	return e.desc
}

func (c *CachingModelStore) store(key cacheKey, desc *spi.ModelDescriptor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{
		desc:      desc,
		expiresAt: c.clock.Now().Add(c.jitteredLeaseLocked()),
	}
}

func (c *CachingModelStore) evict(key cacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *CachingModelStore) invalidate(ctx context.Context, ref spi.ModelRef) {
	key := cacheKey{tenant: tenantOf(ctx), ref: ref}
	c.evict(key)
	c.notifyLocalSubscribers(key.tenant, ref)
	if c.broadcaster != nil {
		payload, err := EncodeInvalidation(key.tenant, ref)
		if err == nil {
			c.broadcaster.Broadcast(topicModelInvalidate, payload)
		}
	}
}

func (c *CachingModelStore) handleInvalidation(payload []byte) {
	tenant, ref, ok := DecodeInvalidation(payload)
	if !ok {
		return
	}
	c.evict(cacheKey{tenant: tenant, ref: ref})
	c.notifyLocalSubscribers(tenant, ref)
}

// SubscribeLocal registers an in-process invalidation handler. The
// handler fires for every invalidation seen by this store —
// originating from a local mutation (Save/Lock/Unlock/SetChangeLevel/
// Delete/ExtendSchema) AND from gossip events received from peer
// nodes. Downstream caches keyed off the same (tenant, ref) tuple
// (e.g. the path-validation negative cache) use this to stay in lock
// step with the descriptor cache regardless of cluster topology.
//
// Issue #174 — pre-fix the path-validation cache subscribed to the
// gossip broadcaster directly, so single-node deployments (where the
// broadcaster is nil) never received any invalidation events.
func (c *CachingModelStore) SubscribeLocal(h func(tenant string, ref spi.ModelRef)) {
	c.localSubMu.Lock()
	defer c.localSubMu.Unlock()
	c.localSubs = append(c.localSubs, h)
}

func (c *CachingModelStore) notifyLocalSubscribers(tenant string, ref spi.ModelRef) {
	c.localSubMu.RLock()
	subs := append([]func(string, spi.ModelRef){}, c.localSubs...)
	c.localSubMu.RUnlock()
	for _, h := range subs {
		h(tenant, ref)
	}
}

// jitteredLeaseLocked returns lease ± 10 %. MUST be called with c.mu
// held (exclusive): jitterRand is not concurrent-safe.
func (c *CachingModelStore) jitteredLeaseLocked() time.Duration {
	factor := 0.9 + 0.2*c.jitterRand.Float64()
	return time.Duration(float64(c.lease) * factor)
}

func flightKey(key cacheKey) string {
	return key.tenant + "|" + key.ref.EntityName + "|" + key.ref.ModelVersion
}

// tenantOf best-effort extracts tenant from ctx; empty string on absence.
func tenantOf(ctx context.Context) string {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return ""
	}
	return string(uc.Tenant.ID)
}

// Compile-time interface check.
var _ spi.ModelStore = (*CachingModelStore)(nil)
