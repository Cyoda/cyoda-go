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
func (s *SearchService) RegisterJobForTest(jobID string, cancel context.CancelCauseFunc, uc *spi.UserContext) bool {
	_, ok := s.registerJob(jobID, cancel, uc, 1)
	return ok
}

// DeregisterJobForTest exposes deregisterJobHandle, the release half of the
// pair above. It looks up jobID's current handle and deregisters by identity,
// matching how the executor's own defer releases its registration.
func (s *SearchService) DeregisterJobForTest(jobID string) {
	s.registryMu.Lock()
	h := s.registry[jobID]
	s.registryMu.Unlock()
	if h != nil {
		s.deregisterJobHandle(jobID, h)
	}
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
// (tenant, ref) buckets the path-validation cache will retain.
// Used by tests to drive the LRU eviction path.
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

// JobAttemptsExhausted returns the caller-facing message the reclaim sweep
// writes when it abandons a job past the attempt cap. Exposed so external
// tests assert against the constant rather than duplicating its text.
func JobAttemptsExhausted() string {
	return jobAttemptsExhausted
}

// ErrJobSuperseded returns the cancellation cause registerReclaim sets on a
// handle it replaces (a self-reclaim: this node re-registers a job it was
// already running). Exposed so an external test can assert
// context.Cause(oldCtx) against the exact sentinel rather than duplicating
// or guessing at its text.
func ErrJobSuperseded() error {
	return errJobSuperseded
}

// AsyncJobHandleForTest is an opaque handle to a registered job's cancel
// entry. It lets an external test hold a SPECIFIC handle instance (as
// returned by RegisterJobHandleForTest/RegisterReclaimForTest) and later
// present exactly that instance to DeregisterJobHandleForTest — as opposed to
// DeregisterJobForTest, which always looks up and deregisters whatever handle
// is CURRENTLY registered for a jobID. That distinction is the point: it is
// what lets a test drive deregisterJobHandle's compare-and-delete identity
// check (a superseded old handle's deregistration must not evict the new
// handle a self-reclaim installed in its place).
type AsyncJobHandleForTest = *asyncJobHandle

// RegisterJobHandleForTest is RegisterJobForTest's sibling: it exposes
// registerJob but returns the created handle (fixed at epoch 1) instead of
// just a bool, so a test can later present that exact handle instance to
// DeregisterJobHandleForTest.
func (s *SearchService) RegisterJobHandleForTest(jobID string, cancel context.CancelCauseFunc, uc *spi.UserContext) AsyncJobHandleForTest {
	h, _ := s.registerJob(jobID, cancel, uc, 1)
	return h
}

// RegisterReclaimForTest exposes registerReclaim so an external test can
// drive the self-reclaim replace path directly: registering a second handle
// for a jobID that already has one, at a given epoch, without a full
// ReclaimStaleJobs round-trip through a store.
func (s *SearchService) RegisterReclaimForTest(jobID string, cancel context.CancelCauseFunc, uc *spi.UserContext, epoch int64) AsyncJobHandleForTest {
	return s.registerReclaim(jobID, cancel, uc, epoch)
}

// DeregisterJobHandleForTest exposes deregisterJobHandle for a specific
// captured handle, rather than DeregisterJobForTest's "look up whatever is
// current" behaviour.
func (s *SearchService) DeregisterJobHandleForTest(jobID string, h AsyncJobHandleForTest) {
	s.deregisterJobHandle(jobID, h)
}
