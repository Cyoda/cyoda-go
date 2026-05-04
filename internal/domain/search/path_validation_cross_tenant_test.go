package search_test

import (
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// TestPathValidationCache_CrossTenantNoisyNeighborSurvives pins
// issue #175. Pre-fix the cache used a single global otter cache
// with MaximumSize=10000 — a tenant-A flooder spamming distinct
// random fieldPaths against `(tenantA, refX)` could S3-FIFO-evict
// every legitimate `(tenantB, refY)` entry from tenant B.
//
// The fix partitions the cache into per-(tenant, ref) buckets with a
// fixed bucket capacity. Eviction within tenant A's bucket cannot
// affect tenant B's bucket. This test asserts that contract: tenant
// B's previously-cached negative entry survives a flood of tenant A
// entries far exceeding the old global capacity.
func TestPathValidationCache_CrossTenantNoisyNeighborSurvives(t *testing.T) {
	cache := search.NewPathValidationCache()

	refA := spi.ModelRef{EntityName: "x", ModelVersion: "1"}
	refB := spi.ModelRef{EntityName: "y", ModelVersion: "1"}

	// Tenant B's legitimate negative entry — must remain cached after
	// the flood.
	cache.MarkAbsent("tenant-B", refB, "$.legit.path")
	if !cache.IsAbsent("tenant-B", refB, "$.legit.path") {
		t.Fatalf("setup: tenant B's entry not stored")
	}

	// Tenant A floods its own bucket far past the per-bucket capacity
	// (and well past the previous global 10000 limit).
	const floodSize = 12000
	for i := 0; i < floodSize; i++ {
		cache.MarkAbsent("tenant-A", refA, fmt.Sprintf("$.flood.%d", i))
	}

	if !cache.IsAbsent("tenant-B", refB, "$.legit.path") {
		t.Errorf("tenant B's entry was evicted by tenant A's flood — cross-tenant isolation broken")
	}
}

// TestPathValidationCache_PerBucketCapacityIsolatesWithinTenant pins
// the second half of #175's contract — eviction inside tenant A's
// bucket only affects tenant A's own entries within that (tenant, ref).
// The flood evicts tenant A's own earlier entries, but does not reach
// across the (tenant, ref) partition into tenant B's data.
func TestPathValidationCache_PerBucketCapacityIsolatesWithinTenant(t *testing.T) {
	cache := search.NewPathValidationCache()

	refA := spi.ModelRef{EntityName: "x", ModelVersion: "1"}
	refB := spi.ModelRef{EntityName: "y", ModelVersion: "1"}

	cache.MarkAbsent("tenant-B", refB, "$.b.legit")

	// Within tenant-A's (tenant, ref) bucket, generate enough
	// distinct paths to exhaust the per-bucket capacity. Internal
	// otter S3-FIFO will reap the oldest entries within this bucket;
	// other buckets are untouched.
	const overflow = 10000
	for i := 0; i < overflow; i++ {
		cache.MarkAbsent("tenant-A", refA, fmt.Sprintf("$.path.%d", i))
	}

	// Tenant B's bucket is unaffected.
	if !cache.IsAbsent("tenant-B", refB, "$.b.legit") {
		t.Errorf("tenant B's bucket affected by tenant A's overflow")
	}
}
