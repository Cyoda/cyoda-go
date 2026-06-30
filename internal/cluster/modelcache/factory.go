package modelcache

import (
	"context"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// CachingStoreFactory wraps an spi.StoreFactory so that ModelStore(ctx)
// returns a per-request store whose Get / RefreshAndGet calls route
// through a single shared cache. All other factory methods pass
// through to the inner factory unchanged.
//
// The cache is gossip-aware: mutations on a peer node evict the local
// entry via the "model.invalidate" topic. The ±10% jittered TTL is the
// fallback when gossip drops a message.
type CachingStoreFactory struct {
	inner spi.StoreFactory
	cache *CachingModelStore // state-only — inner ModelStore is unused on this instance
}

// NewCachingStoreFactory wraps inner. broadcaster may be nil for
// single-node deployments. clk may be nil to use the wall clock.
// lease must be > 0.
func NewCachingStoreFactory(
	inner spi.StoreFactory,
	broadcaster spi.ClusterBroadcaster,
	clk Clock,
	lease time.Duration,
) *CachingStoreFactory {
	// The cache instance's "inner" is never called — per-request stores
	// supply their own inner via requestScopedStore.Get. Pass nil and
	// rely on the fact that CachingModelStore.lookup / store / evict
	// never dereference inner.
	cache := New(nil, broadcaster, clk, lease)
	return &CachingStoreFactory{inner: inner, cache: cache}
}

// ModelStore returns a per-request store that reads through the
// shared cache and delegates mutations to the inner per-tenant store.
func (f *CachingStoreFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	innerStore, err := f.inner.ModelStore(ctx)
	if err != nil {
		return nil, err
	}
	return &requestScopedStore{
		cache: f.cache,
		inner: innerStore,
	}, nil
}

// Pass-throughs for the rest of spi.StoreFactory — the cache only
// decorates ModelStore.

func (f *CachingStoreFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	return f.inner.EntityStore(ctx)
}
func (f *CachingStoreFactory) KeyValueStore(ctx context.Context) (spi.KeyValueStore, error) {
	return f.inner.KeyValueStore(ctx)
}
func (f *CachingStoreFactory) MessageStore(ctx context.Context) (spi.MessageStore, error) {
	return f.inner.MessageStore(ctx)
}
func (f *CachingStoreFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	return f.inner.WorkflowStore(ctx)
}
func (f *CachingStoreFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	return f.inner.StateMachineAuditStore(ctx)
}
func (f *CachingStoreFactory) AsyncSearchStore(ctx context.Context) (spi.AsyncSearchStore, error) {
	return f.inner.AsyncSearchStore(ctx)
}
func (f *CachingStoreFactory) TransactionManager(ctx context.Context) (spi.TransactionManager, error) {
	return f.inner.TransactionManager(ctx)
}
func (f *CachingStoreFactory) Close() error { return f.inner.Close() }

// SupportsCompositeUniqueKeys forwards the optional capability check to the
// inner factory. Returns false if the inner factory does not implement
// spi.CompositeUniqueKeyCapable.
func (f *CachingStoreFactory) SupportsCompositeUniqueKeys() bool {
	if c, ok := f.inner.(spi.CompositeUniqueKeyCapable); ok {
		return c.SupportsCompositeUniqueKeys()
	}
	return false
}

// SubscribeLocal registers an in-process invalidation handler on the
// shared CachingModelStore. The handler receives (tenant, ref) for
// every model invalidation — local mutations and gossip-received
// events alike. Downstream caches use this to stay in lock step
// regardless of cluster topology (issue #174).
func (f *CachingStoreFactory) SubscribeLocal(h func(tenant string, ref spi.ModelRef)) {
	f.cache.SubscribeLocal(h)
}

// requestScopedStore is returned by CachingStoreFactory.ModelStore.
// It delegates reads through the shared cache and writes directly to
// the per-request inner store. The tenant is implicit in inner — the
// inner is already tenant-scoped at construction — and the cache
// keys off tenant-from-ctx internally.
type requestScopedStore struct {
	cache *CachingModelStore
	inner spi.ModelStore
}

// Compile-time interface check.
var _ spi.ModelStore = (*requestScopedStore)(nil)

func (s *requestScopedStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	key := cacheKey{tenant: common.TenantFromContext(ctx), ref: ref}
	if d := s.cache.lookup(key); d != nil {
		return d, nil
	}
	desc, err := s.inner.Get(ctx, ref)
	if err != nil || desc == nil {
		return desc, err
	}
	if desc.State != spi.ModelLocked {
		return desc, nil
	}
	s.cache.store(key, desc)
	return desc, nil
}

// RefreshAndGet forces a cache miss, collapses concurrent callers via
// the cache's singleflight group, and repopulates on success.
func (s *requestScopedStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	key := cacheKey{tenant: common.TenantFromContext(ctx), ref: ref}
	s.cache.evict(key)

	v, err, _ := s.cache.flight.Do(flightKey(key), func() (any, error) {
		desc, err := s.inner.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		if desc != nil && desc.State == spi.ModelLocked {
			s.cache.store(key, desc)
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

func (s *requestScopedStore) Save(ctx context.Context, desc *spi.ModelDescriptor) error {
	if err := s.inner.Save(ctx, desc); err != nil {
		return err
	}
	s.cache.invalidate(ctx, desc.Ref)
	return nil
}

func (s *requestScopedStore) Lock(ctx context.Context, ref spi.ModelRef) error {
	if err := s.inner.Lock(ctx, ref); err != nil {
		return err
	}
	s.cache.invalidate(ctx, ref)
	return nil
}

func (s *requestScopedStore) Unlock(ctx context.Context, ref spi.ModelRef) error {
	if err := s.inner.Unlock(ctx, ref); err != nil {
		return err
	}
	s.cache.invalidate(ctx, ref)
	return nil
}

func (s *requestScopedStore) SetChangeLevel(ctx context.Context, ref spi.ModelRef, level spi.ChangeLevel) error {
	if err := s.inner.SetChangeLevel(ctx, ref, level); err != nil {
		return err
	}
	s.cache.invalidate(ctx, ref)
	return nil
}

func (s *requestScopedStore) Delete(ctx context.Context, ref spi.ModelRef) error {
	if err := s.inner.Delete(ctx, ref); err != nil {
		return err
	}
	s.cache.invalidate(ctx, ref)
	return nil
}

func (s *requestScopedStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if err := s.inner.ExtendSchema(ctx, ref, delta); err != nil {
		return err
	}
	s.cache.invalidate(ctx, ref)
	return nil
}

func (s *requestScopedStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	return s.inner.GetAll(ctx)
}

func (s *requestScopedStore) IsLocked(ctx context.Context, ref spi.ModelRef) (bool, error) {
	return s.inner.IsLocked(ctx, ref)
}
