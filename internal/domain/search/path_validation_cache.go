package search

import (
	"sync"

	"github.com/maypok86/otter/v2"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// pathValidationBucketCapacity bounds the number of distinct
// negative-cache entries the cache holds for any single (tenant, ref)
// model. Otter's S3-FIFO eviction handles overflow within the bucket.
//
// Issue #175 — pre-fix the cache used a single global otter cache
// with MaximumSize=10000; an adversarial tenant could fill it with
// 10001 distinct random fieldPaths under their own (tenant, ref) and
// S3-FIFO-evict every other tenant's legitimate entries. Per-bucket
// capacity isolates eviction within a single (tenant, ref): a
// flooding tenant only evicts their own data.
//
// 100 entries per bucket is conservative — most production workloads
// see a small set of repeatedly-queried unknown paths per model, not
// thousands. Tune up if observability shows benign workloads hitting
// the cap.
const pathValidationBucketCapacity = 100

// modelRefKey is the (tenant, modelRef) bucket key. One otter cache
// per bucket; entire-bucket eviction handled by InvalidateRef.
type modelRefKey struct {
	tenant       string
	entityName   string
	modelVersion string
}

// PathValidationCache is a loading-cache-style negative cache for the
// search-service's pre-execution field-path validation. It records
// "this path was confirmed absent from this (tenant, modelRef)'s
// schema FieldsMap". A serial flood of validation requests for the
// same unknown path collapses into a single inner-store Get +
// RefreshAndGet pair instead of one pair per request.
//
// Invalidation is driven externally — call InvalidateRef(tenant, ref)
// when a schema change for that model lands. The cache holds no
// reference to the descriptor cache or the cluster broadcaster
// directly: app wiring connects it to either or both via
// modelcache.CachingStoreFactory.SubscribeLocal (issue #174 — local
// mutations AND gossip events both reach this cache, on every
// cluster topology).
//
// Each (tenant, ref) lives in its own bounded otter cache (issue #175
// — per-bucket capacity isolates cross-tenant eviction). InvalidateRef
// drops the bucket entirely.
//
// The zero value is not safe — use NewPathValidationCache.
type PathValidationCache struct {
	mu      sync.Mutex
	buckets map[modelRefKey]*otter.Cache[string, struct{}]
}

// NewPathValidationCache constructs an empty PathValidationCache.
// Callers wire invalidation externally via
// modelcache.CachingStoreFactory.SubscribeLocal.
func NewPathValidationCache() *PathValidationCache {
	return &PathValidationCache{
		buckets: make(map[modelRefKey]*otter.Cache[string, struct{}]),
	}
}

// IsAbsent reports whether the (tenant, ref, path) tuple has a
// current negative-cache entry — i.e. the path was previously
// confirmed absent from the model's FieldsMap and no schema-change
// invalidation has fired for that model since.
//
// The lookup AND the otter GetIfPresent call run under c.mu so a
// concurrent InvalidateRef cannot drop the bucket between the two
// steps (review #211 — TOCTOU concern). otter.Cache is internally
// thread-safe; the mutex is about preserving the "operate on the
// bucket I just resolved from c.buckets" invariant, not about
// otter's own concurrency.
func (c *PathValidationCache) IsAbsent(tenant string, ref spi.ModelRef, path string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := c.bucketLocked(modelRefKey{
		tenant:       tenant,
		entityName:   ref.EntityName,
		modelVersion: ref.ModelVersion,
	}, false /* createIfMissing */)
	if bucket == nil {
		return false
	}
	_, ok := bucket.GetIfPresent(path)
	return ok
}

// MarkAbsent records the (tenant, ref, path) tuple as confirmed
// absent. Subsequent IsAbsent calls return true until either the
// path's bucket is invalidated via InvalidateRef or otter's S3-FIFO
// eviction reaps the entry under bucket-internal pressure.
//
// Lock held across both bucket allocation and Set so a concurrent
// InvalidateRef can't drop the bucket out from under us — without
// the lock the Set would land in an orphaned bucket (review #211).
func (c *PathValidationCache) MarkAbsent(tenant string, ref spi.ModelRef, path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := c.bucketLocked(modelRefKey{
		tenant:       tenant,
		entityName:   ref.EntityName,
		modelVersion: ref.ModelVersion,
	}, true /* createIfMissing */)
	bucket.Set(path, struct{}{})
}

// MarkPresent removes any negative cache entry for the (tenant, ref,
// path) tuple. Defensive: callers invoke this when a path has been
// confirmed present in the schema, ensuring a follow-up rename or
// schema fix is reflected immediately rather than waiting for an
// invalidation event.
func (c *PathValidationCache) MarkPresent(tenant string, ref spi.ModelRef, path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := c.bucketLocked(modelRefKey{
		tenant:       tenant,
		entityName:   ref.EntityName,
		modelVersion: ref.ModelVersion,
	}, false)
	if bucket == nil {
		return
	}
	bucket.Invalidate(path)
}

// InvalidateRef drops every cached negative entry for (tenant, ref).
// The whole bucket is removed from the map; the next MarkAbsent for
// that bucket allocates a fresh otter cache.
//
// This is the public hook wired up by app.go to
// modelcache.CachingStoreFactory.SubscribeLocal so the cache reacts
// to local mutations AND remote gossip events alike.
func (c *PathValidationCache) InvalidateRef(tenant string, ref spi.ModelRef) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.buckets, modelRefKey{
		tenant:       tenant,
		entityName:   ref.EntityName,
		modelVersion: ref.ModelVersion,
	})
}

// bucketLocked returns the bucket for k. Caller MUST hold c.mu — the
// returned otter.Cache pointer is only safe to operate on while c.mu
// is held, since InvalidateRef may delete the map entry concurrently
// otherwise (review #211).
func (c *PathValidationCache) bucketLocked(k modelRefKey, createIfMissing bool) *otter.Cache[string, struct{}] {
	b, ok := c.buckets[k]
	if ok {
		return b
	}
	if !createIfMissing {
		return nil
	}
	b = otter.Must(&otter.Options[string, struct{}]{
		MaximumSize: pathValidationBucketCapacity,
	})
	c.buckets[k] = b
	return b
}
