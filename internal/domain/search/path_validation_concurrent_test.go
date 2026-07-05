package search_test

import (
	"fmt"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// TestPathValidationCache_ConcurrentMarkAbsentAndInvalidateRef stresses
// the per-bucket invalidation path against the reviewer's TOCTOU
// concern (#211 review feedback). Pre-fix, MarkAbsent acquired c.mu
// briefly (in bucketFor) to obtain a *otter.Cache pointer, released
// the lock, then called bucket.Set without holding c.mu. A concurrent
// InvalidateRef could delete(c.buckets, k) between those two steps
// and the subsequent Set would land in an orphaned bucket (still
// correct end-state, but only by accident — a tail of pre-invalidation
// writes could end up in the dropped bucket while readers used the
// fresh one).
//
// Post-fix bucketFor holds c.mu across both lookup AND cache op so the
// "operate on the bucket I just fetched" sequence is atomic relative
// to InvalidateRef. This test runs heavy concurrent MarkAbsent +
// InvalidateRef + IsAbsent + MarkPresent traffic on a small set of
// (tenant, ref) keys; under -race any inconsistency would surface.
//
// We don't (and can't reliably) assert the orphan-write bug
// deterministically — the timing window is microseconds and the
// orphan is unobservable from outside. The test's value is: it runs
// the post-fix code through a contention pattern that the existing
// tests don't, and a future regression that loosens the locking
// discipline gets caught here under -race.
func TestPathValidationCache_ConcurrentMarkAbsentAndInvalidateRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent stress test under -short")
	}
	cache := search.NewPathValidationCache()

	const (
		tenants      = 4
		refsPerKey   = 3
		pathsPerOp   = 8
		iterations   = 200
		workersPerOp = 4
	)

	refs := make([]spi.ModelRef, refsPerKey)
	for i := range refs {
		refs[i] = spi.ModelRef{EntityName: fmt.Sprintf("e%d", i), ModelVersion: "1"}
	}

	var wg sync.WaitGroup
	for w := 0; w < workersPerOp; w++ {
		// MarkAbsent flooders.
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				tenant := fmt.Sprintf("t%d", (seed+i)%tenants)
				ref := refs[(seed+i)%refsPerKey]
				for p := 0; p < pathsPerOp; p++ {
					cache.MarkAbsent(tenant, ref, fmt.Sprintf("$.field.%d.%d", seed, p))
				}
			}
		}(w)

		// InvalidateRef storms.
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				tenant := fmt.Sprintf("t%d", (seed+i)%tenants)
				ref := refs[(seed+i)%refsPerKey]
				cache.InvalidateRef(tenant, ref)
			}
		}(w)

		// IsAbsent readers.
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				tenant := fmt.Sprintf("t%d", (seed+i)%tenants)
				ref := refs[(seed+i)%refsPerKey]
				_ = cache.IsAbsent(tenant, ref, fmt.Sprintf("$.field.%d.0", seed))
			}
		}(w)

		// MarkPresent prunes.
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				tenant := fmt.Sprintf("t%d", (seed+i)%tenants)
				ref := refs[(seed+i)%refsPerKey]
				cache.MarkPresent(tenant, ref, fmt.Sprintf("$.field.%d.0", seed))
			}
		}(w)
	}
	wg.Wait()

	// Final invariant: after every InvalidateRef+MarkPresent pair the
	// cache should be in a consistent state for any (tenant, ref).
	// Specifically a freshly-invalidated bucket reports IsAbsent=false
	// for any path until the next MarkAbsent.
	for ti := 0; ti < tenants; ti++ {
		for _, ref := range refs {
			tenant := fmt.Sprintf("t%d", ti)
			cache.InvalidateRef(tenant, ref)
			if cache.IsAbsent(tenant, ref, "$.never.marked") {
				t.Errorf("after InvalidateRef, IsAbsent must return false for unwritten path; got true for tenant=%s ref=%v", tenant, ref)
			}
		}
	}
}
