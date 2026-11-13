package modelcache

import (
	"context"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// CachingStoreFactory wraps an spi.StoreFactory so that ModelStore(ctx)
// returns a per-request store whose reads and extensions route through one
// shared cache. All other factory methods pass through to the inner factory
// unchanged.
//
// The cache is gossip-aware: mutations on a peer node evict the local
// entry via the "model.invalidate" topic. The ±10% jittered TTL is the
// fallback when gossip drops a message.
type CachingStoreFactory struct {
	inner spi.StoreFactory
	cache *CachingModelStore
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
	return &CachingStoreFactory{inner: inner, cache: New(broadcaster, clk, lease)}
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
func (f *CachingStoreFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	return f.inner.ScheduledTaskStore(ctx)
}
func (f *CachingStoreFactory) TransactionManager(ctx context.Context) (spi.TransactionManager, error) {
	return f.inner.TransactionManager(ctx)
}
func (f *CachingStoreFactory) Close() error { return f.inner.Close() }

// Ping forwards to the inner factory.
func (f *CachingStoreFactory) Ping(ctx context.Context) error { return f.inner.Ping(ctx) }

// SupportsCompositeUniqueKeys forwards the optional capability check to the
// inner factory. Returns false if the inner factory does not implement
// spi.CompositeUniqueKeyCapable.
func (f *CachingStoreFactory) SupportsCompositeUniqueKeys() bool {
	if c, ok := f.inner.(spi.CompositeUniqueKeyCapable); ok {
		return c.SupportsCompositeUniqueKeys()
	}
	return false
}

// TransactionCoordinator forwards the optional transaction-coordination
// capability to the inner factory. It returns (nil, nil), which means "not
// capable", when the inner factory does not implement
// spi.TransactionCoordinationCapable.
func (f *CachingStoreFactory) TransactionCoordinator(ctx context.Context) (spi.TransactionCoordinator, error) {
	if c, ok := f.inner.(spi.TransactionCoordinationCapable); ok {
		return c.TransactionCoordinator(ctx)
	}
	return nil, nil
}

// requestScopedStore is returned by CachingStoreFactory.ModelStore.
// It reads through the shared cache and writes to the per-request inner
// store, invalidating the shared entry after every write. The tenant is
// implicit in inner — the inner is already tenant-scoped at construction —
// and the cache keys off tenant-from-ctx.
type requestScopedStore struct {
	cache *CachingModelStore
	inner spi.ModelStore
}

// Compile-time interface check.
var _ spi.ModelStore = (*requestScopedStore)(nil)

func keyOf(ctx context.Context, ref spi.ModelRef) cacheKey {
	return cacheKey{tenant: common.TenantFromContext(ctx), ref: ref}
}

// Get serves a live cached entry, otherwise reads storage through the shared
// (key, generation) flight.
func (s *requestScopedStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	key := keyOf(ctx, ref)
	if e, ok := s.cache.lookupEntry(key); ok {
		return e.desc, nil
	}
	return s.cache.readThrough(ctx, key, s.inner)
}

// SchemaNode returns ref's parsed model schema, derived once per cache entry
// and reused for that entry's lifetime. Returns (nil, nil) when the descriptor
// has no schema bound.
//
// A descriptor that was not cached — UNLOCKED, or a fill the generation guard
// dropped — is parsed fresh and not retained, as its bytes are not.
//
// The returned node is SHARED: for a cached entry every reader gets the same
// tree, and a ModelNode is not safe for concurrent mutation. Treat it as
// read-only.
func (s *requestScopedStore) SchemaNode(ctx context.Context, ref spi.ModelRef) (*schema.ModelNode, error) {
	key := keyOf(ctx, ref)
	if e, ok := s.cache.lookupEntry(key); ok {
		return e.node, e.nodeErr
	}
	desc, err := s.cache.readThrough(ctx, key, s.inner)
	if err != nil {
		return nil, err
	}
	if e, ok := s.cache.lookupEntry(key); ok && e.desc == desc {
		return e.node, e.nodeErr
	}
	return parseSchemaNode(desc)
}

// RefreshAndGet reads storage through the same (key, generation) flight as a
// miss, skipping the cached entry, and caches the result under the fill guard.
// It neither evicts nor bumps, so a stream of refreshes does not empty the
// cache for the readers it serves meanwhile.
func (s *requestScopedStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	return s.cache.readThrough(ctx, keyOf(ctx, ref), s.inner)
}

// ExtendSchema shares one storage call among concurrent identical extensions
// on this node, and invalidates the entry after the call whatever its outcome.
// See CachingModelStore.extend.
func (s *requestScopedStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	return s.cache.extend(ctx, keyOf(ctx, ref), delta, s.inner)
}

func (s *requestScopedStore) Save(ctx context.Context, desc *spi.ModelDescriptor) error {
	if err := s.inner.Save(ctx, desc); err != nil {
		return err
	}
	s.cache.invalidate(keyOf(ctx, desc.Ref))
	return nil
}

func (s *requestScopedStore) Lock(ctx context.Context, ref spi.ModelRef) error {
	if err := s.inner.Lock(ctx, ref); err != nil {
		return err
	}
	s.cache.invalidate(keyOf(ctx, ref))
	return nil
}

func (s *requestScopedStore) Unlock(ctx context.Context, ref spi.ModelRef) error {
	if err := s.inner.Unlock(ctx, ref); err != nil {
		return err
	}
	s.cache.invalidate(keyOf(ctx, ref))
	return nil
}

func (s *requestScopedStore) SetChangeLevel(ctx context.Context, ref spi.ModelRef, level spi.ChangeLevel) error {
	if err := s.inner.SetChangeLevel(ctx, ref, level); err != nil {
		return err
	}
	s.cache.invalidate(keyOf(ctx, ref))
	return nil
}

func (s *requestScopedStore) Delete(ctx context.Context, ref spi.ModelRef) error {
	if err := s.inner.Delete(ctx, ref); err != nil {
		return err
	}
	s.cache.invalidate(keyOf(ctx, ref))
	return nil
}

func (s *requestScopedStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	return s.inner.GetAll(ctx)
}

func (s *requestScopedStore) IsLocked(ctx context.Context, ref spi.ModelRef) (bool, error) {
	return s.inner.IsLocked(ctx, ref)
}
