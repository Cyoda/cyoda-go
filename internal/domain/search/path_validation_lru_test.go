package search_test

import (
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// TestPathValidationCache_BucketMapEvictsLRU pins issue #218: the
// per-(tenant, ref) bucket map must enforce a maximum size to bound
// memory under adversarial workloads (a tenant with model-creation
// privilege at scale who searches against many distinct models).
//
// Pre-fix the map was unbounded; only the per-bucket otter cache
// was capped. Post-fix the cache evicts least-recently-used buckets
// when len(buckets) exceeds the configured cap.
//
// This test floods the cache with > cap distinct (tenant, ref) keys
// and asserts:
//  1. len(buckets) stays at the cap (no unbounded growth);
//  2. recently-used buckets survive (LRU property);
//  3. oldest unused buckets are the ones evicted.
func TestPathValidationCache_BucketMapEvictsLRU(t *testing.T) {
	cap := search.PathValidationBucketMapCap()
	if cap <= 1 {
		t.Fatalf("PathValidationBucketMapCap() = %d, want > 1", cap)
	}

	cache := search.NewPathValidationCache()

	// Insert cap+50 distinct buckets. The first 50 should be evicted.
	for i := 0; i < cap+50; i++ {
		ref := spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
		cache.MarkAbsent("t", ref, "$.path")
	}

	if got := search.PathValidationCacheBucketCount(cache); got != cap {
		t.Fatalf("bucket count after flood: got %d, want %d (cap)", got, cap)
	}

	// Buckets 0..49 should be evicted (LRU), buckets 50..cap+49 should
	// survive.
	for i := 0; i < 50; i++ {
		ref := spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
		if cache.IsAbsent("t", ref, "$.path") {
			t.Errorf("bucket %d should have been evicted (LRU), but IsAbsent still returns true", i)
		}
	}
	for i := 50; i < cap+50; i++ {
		ref := spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
		if !cache.IsAbsent("t", ref, "$.path") {
			t.Errorf("bucket %d should have survived, but IsAbsent returns false", i)
		}
	}
}

// TestPathValidationCache_LRURecencyTouchedByRead pins that IsAbsent
// promotes the bucket to MRU, so a frequently-read bucket is not
// evicted just because no new MarkAbsent fires for it.
func TestPathValidationCache_LRURecencyTouchedByRead(t *testing.T) {
	cap := search.PathValidationBucketMapCap()
	cache := search.NewPathValidationCache()

	// Seed bucket 0.
	ref0 := spi.ModelRef{EntityName: "e0", ModelVersion: "1"}
	cache.MarkAbsent("t", ref0, "$.path")

	// Fill the rest of the cap.
	for i := 1; i < cap; i++ {
		ref := spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
		cache.MarkAbsent("t", ref, "$.path")
	}

	// At cap. Now read bucket 0 to promote it to MRU.
	if !cache.IsAbsent("t", ref0, "$.path") {
		t.Fatalf("bucket 0 should be present before LRU touch")
	}

	// Add cap-1 more buckets. Without the IsAbsent touch, bucket 0
	// would be evicted by the very first new insert (it was the
	// oldest). With the touch promoting bucket 0 to MRU, bucket 1
	// becomes the LRU and is evicted by the first insert; bucket 0
	// survives all cap-1 subsequent inserts because we don't push
	// it past the back of the list.
	for i := cap; i < 2*cap-1; i++ {
		ref := spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
		cache.MarkAbsent("t", ref, "$.path")
	}

	if !cache.IsAbsent("t", ref0, "$.path") {
		t.Errorf("bucket 0 should have survived because IsAbsent touched it (LRU)")
	}
	ref1 := spi.ModelRef{EntityName: "e1", ModelVersion: "1"}
	if cache.IsAbsent("t", ref1, "$.path") {
		t.Errorf("bucket 1 should have been evicted (oldest after bucket 0 was touched)")
	}
}

// TestPathValidationCache_InvalidateRefDoesNotTouchLRU pins that
// InvalidateRef drops the bucket but does NOT update LRU recency
// (treating invalidation as orthogonal to access). Otherwise an
// adversary could spam invalidations to keep their own buckets MRU
// at the expense of legitimate tenants.
func TestPathValidationCache_InvalidateRefDoesNotTouchLRU(t *testing.T) {
	cap := search.PathValidationBucketMapCap()
	cache := search.NewPathValidationCache()

	// Seed and immediately invalidate bucket 0 — bucket 0 is now
	// gone from the map entirely (InvalidateRef drops the bucket).
	ref0 := spi.ModelRef{EntityName: "e0", ModelVersion: "1"}
	cache.MarkAbsent("t", ref0, "$.path")
	cache.InvalidateRef("t", ref0)

	// Fill cap buckets fresh.
	for i := 1; i <= cap; i++ {
		ref := spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
		cache.MarkAbsent("t", ref, "$.path")
	}

	// Add one more — bucket 1 should be evicted (oldest), not e.g.
	// some bucket that would have been "kept alive" by the earlier
	// InvalidateRef on bucket 0.
	cache.MarkAbsent("t", spi.ModelRef{EntityName: "eN", ModelVersion: "1"}, "$.path")

	if cache.IsAbsent("t", spi.ModelRef{EntityName: "e1", ModelVersion: "1"}, "$.path") {
		t.Errorf("bucket 1 should have been evicted as LRU; InvalidateRef should not promote anything")
	}
}
