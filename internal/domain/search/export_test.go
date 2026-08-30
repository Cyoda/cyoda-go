package search

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Test-only accessors. The build tag `*_test.go` keeps these out of
// the production binary while making them visible to external tests
// in the search_test package.

// RegisterJobForTest exposes registerJob so an external test can drive the
// per-tenant registry directly, without a submit round-trip. Used to pin the
// accounting invariants (one slot per registered job, no double-count on a
// duplicate jobID) structurally rather than through the single call site that
// happens to guarantee unique ids today.
func (s *SearchService) RegisterJobForTest(jobID string, cancel context.CancelFunc, uc *spi.UserContext) bool {
	return s.registerJob(jobID, cancel, uc)
}

// DeregisterJobForTest exposes deregisterJob, the release half of the pair
// above.
func (s *SearchService) DeregisterJobForTest(jobID string) {
	s.deregisterJob(jobID)
}

// TenantInFlightForTest returns tenant's current in-flight count, the quantity
// the per-tenant cap is enforced against.
func (s *SearchService) TenantInFlightForTest(tenant spi.TenantID) int {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	return s.tenantInFlight[tenant]
}

// RegisteredJobCountForTest returns the number of live cancel handles.
func (s *SearchService) RegisteredJobCountForTest() int {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	return len(s.registry)
}

// PathValidationBucketMapCap returns the configured maximum number of
// (tenant, ref) buckets the path-validation cache will retain. Issue
// #218 — used by tests to drive the LRU eviction path.
func PathValidationBucketMapCap() int {
	return pathValidationBucketMapCap
}

// PathValidationCacheBucketCount returns the current number of
// non-empty buckets in the cache. Test-only introspection used by
// the LRU stress tests.
func PathValidationCacheBucketCount(c *PathValidationCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buckets)
}

// ResolveSortKeysForTest exposes resolveSortKeys so an external test can
// drive its bounded-refresh contract directly, the same way
// TestSearch_StaleSchema_RefreshesOnceAndSucceeds (path_validate_test.go)
// drives Search's condition-path validation.
func (s *SearchService) ResolveSortKeysForTest(ctx context.Context, modelRef spi.ModelRef, keys []OrderKey) ([]spi.OrderSpec, error) {
	return s.resolveSortKeys(ctx, modelRef, keys)
}

// JobFailureFallback returns the sanitised message written into a job
// record on an unattributable failure — the same constant FailStaleJobs
// (reaper.go) and the executor's own failure paths (service.go) both use.
// Exposed so external tests can assert against the constant itself rather
// than duplicating its literal text.
func JobFailureFallback() string {
	return jobFailureFallback
}
