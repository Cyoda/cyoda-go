package search

// Test-only accessors. The build tag `*_test.go` keeps these out of
// the production binary while making them visible to external tests
// in the search_test package.

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

// JobFailureFallback returns the sanitised message written into a job
// record on an unattributable failure — the same constant FailStaleJobs
// (reaper.go) and the executor's own failure paths (service.go) both use.
// Exposed so external tests can assert against the constant itself rather
// than duplicating its literal text.
func JobFailureFallback() string {
	return jobFailureFallback
}
