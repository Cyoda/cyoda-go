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
// read flight (readThrough).
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
	return s.nodeOf(key, desc)
}

// RefreshAndGet reads storage through the same shared read flight as a miss,
// skipping the cached entry, and caches the result under the fill guard. It
// neither evicts nor bumps, so a stream of refreshes does not empty the
// cache for the readers it serves meanwhile.
func (s *requestScopedStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	return s.cache.readThrough(ctx, keyOf(ctx, ref), s.inner)
}

// RefreshSchemaNode is RefreshAndGet returning the parsed schema of what it
// read, as SchemaNode returns it. A descriptor the cache did not keep because
// it is UNLOCKED is parsed, not read again. Only when an invalidation landed
// during the read, so that what it read may predate the change, does it read
// once more, through SchemaNode: the schema it returns is then at least as new
// as that invalidation.
func (s *requestScopedStore) RefreshSchemaNode(ctx context.Context, ref spi.ModelRef) (*schema.ModelNode, error) {
	key := keyOf(ctx, ref)
	gen := s.cache.generation(key)
	desc, err := s.cache.readThrough(ctx, key, s.inner)
	if err != nil {
		return nil, err
	}
	if s.cache.generation(key) != gen {
		return s.SchemaNode(ctx, ref)
	}
	return s.nodeOf(key, desc)
}

// nodeOf returns the parse of desc, a descriptor just read for key: the cached
// entry's node when the cache holds that same descriptor, else a fresh parse
// that is not retained.
func (s *requestScopedStore) nodeOf(key cacheKey, desc *spi.ModelDescriptor) (*schema.ModelNode, error) {
	if e, ok := s.cache.lookupEntry(key); ok && e.desc == desc {
		return e.node, e.nodeErr
	}
	return parseSchemaNode(desc)
}

// ExtendSchema shares one storage call among concurrent identical extensions
// on this node, and invalidates the entry after the call whatever its outcome.
// See CachingModelStore.extend.
func (s *requestScopedStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	return s.cache.extend(ctx, keyOf(ctx, ref), delta, s.inner)
}

// Model administration drops the cached entry whatever the outcome: a failed
// call may still have committed (a COMMIT whose outcome is unknown), and a
// needless re-read costs one storage read.
func (s *requestScopedStore) Save(ctx context.Context, desc *spi.ModelDescriptor) error {
	defer s.cache.invalidate(keyOf(ctx, desc.Ref))
	return s.inner.Save(ctx, desc)
}

func (s *requestScopedStore) Lock(ctx context.Context, ref spi.ModelRef) error {
	defer s.cache.invalidate(keyOf(ctx, ref))
	return s.inner.Lock(ctx, ref)
}

func (s *requestScopedStore) Unlock(ctx context.Context, ref spi.ModelRef) error {
	defer s.cache.invalidate(keyOf(ctx, ref))
	return s.inner.Unlock(ctx, ref)
}

func (s *requestScopedStore) SetChangeLevel(ctx context.Context, ref spi.ModelRef, level spi.ChangeLevel) error {
	defer s.cache.invalidate(keyOf(ctx, ref))
	return s.inner.SetChangeLevel(ctx, ref, level)
}

func (s *requestScopedStore) Delete(ctx context.Context, ref spi.ModelRef) error {
	defer s.cache.invalidate(keyOf(ctx, ref))
	return s.inner.Delete(ctx, ref)
}

func (s *requestScopedStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	return s.inner.GetAll(ctx)
}

func (s *requestScopedStore) IsLocked(ctx context.Context, ref spi.ModelRef) (bool, error) {
	return s.inner.IsLocked(ctx, ref)
}
