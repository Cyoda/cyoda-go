package search_test

// Driving tests for the stale-job RECLAIM reaper (task 8, extended by task
// 11): ReclaimStaleJobs claims stale/released RUNNING async-search jobs and
// RE-EXECUTES them on this node, or FAILS those past the attempt cap (a bound
// on StaleClaims, executor losses). Beyond the two happy/attempt-cap drivers
// above, this file also covers: a graceful release never counting as an
// attempt, a saturated node claiming nothing, a ClearResults failure
// releasing rather than failing or silently dropping the job, and the
// self-reclaim handle-replace branch (registerReclaim). The self-executing
// skip and WorkerPool.Cap() live in their own files
// (reaper_self_executing_test.go, pool_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newReclaimTestService builds a memory-backed SearchService with a bounded
// pool so the reclaim sweep can actually re-execute a claimed job end to end.
// The pool is drained at cleanup so no worker goroutines outlive the test.
func newReclaimTestService(t *testing.T) (*search.SearchService, *memory.StoreFactory, spi.AsyncSearchStore) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	store, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(50 * time.Millisecond)
	return svc, factory, store
}

// createStaleReclaimJob persists a RUNNING job whose CreateTime is an hour in
// the past (so it is stale against any staleAfter well under an hour) and no
// heartbeat, with the condition and options envelope SubmitAsync itself
// persists — so decodeStoredJob can reconstruct and re-run it.
func createStaleReclaimJob(t *testing.T, store spi.AsyncSearchStore, tenant spi.TenantID, id string, ref spi.ModelRef, cond predicate.Condition, pit time.Time) {
	t.Helper()
	condJSON, err := json.Marshal(cond)
	if err != nil {
		t.Fatalf("marshal condition: %v", err)
	}
	optsJSON, err := json.Marshal(struct {
		Limit       int             `json:"limit"`
		PointInTime *time.Time      `json:"pointInTime,omitempty"`
		OrderBy     []spi.OrderSpec `json:"orderBy,omitempty"`
	}{Limit: 10, PointInTime: &pit})
	if err != nil {
		t.Fatalf("marshal opts: %v", err)
	}
	job := &spi.SearchJob{
		ID:         id,
		TenantID:   tenant,
		Status:     "RUNNING",
		ModelRef:   ref,
		Condition:  condJSON,
		SearchOpts: optsJSON,
		CreateTime: time.Now().Add(-time.Hour),
	}
	if err := store.CreateJob(tenantCtx(string(tenant)), job); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

// (a) a stale job with StaleClaims below the attempt cap is re-enqueued and
// runs to SUCCESSFUL on this node — a crashed node's job is completed by a
// live node, not failed.
func TestReclaimStaleJobs_ReenqueuesStaleJobToSuccessful(t *testing.T) {
	svc, factory, store := newReclaimTestService(t)
	ctx := tenantCtx("tenant-a")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))
	saveEntity(t, ctx, factory, ref, "e2", []byte(`{"name":"Bob"}`))

	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, store, "tenant-a", "job-stale", ref, cond, time.Now())

	reenq, failed, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if reenq != 1 || failed != 0 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, failed %d), want (1, 0)", reenq, failed)
	}

	status := pollUntilTerminal(t, svc, ctx, "job-stale", 5*time.Second)
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("reclaimed job status = %q, want SUCCESSFUL", status.Status)
	}

	ids, total, err := store.GetResultIDs(ctx, "job-stale", 0, 10)
	if err != nil {
		t.Fatalf("GetResultIDs: %v", err)
	}
	if total != 1 || len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("results = %v (total %d), want [e1]", ids, total)
	}
}

// (b) a job at the attempt cap (the staleness claim brings StaleClaims up to
// maxAttempts) is FAILED with the jobAttemptsExhausted message, not re-run.
func TestReclaimStaleJobs_AttemptCapFailsJob(t *testing.T) {
	svc, _, store := newReclaimTestService(t)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, store, "tenant-a", "job-cap", ref, cond, time.Now())

	// maxAttempts=1: this first staleness claim bumps StaleClaims to 1, which
	// meets the cap, so the job is abandoned (FAILED) rather than re-run.
	reenq, failed, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 1)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if reenq != 0 || failed != 1 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, failed %d), want (0, 1)", reenq, failed)
	}

	got, err := store.GetJob(tenantCtx("tenant-a"), "job-cap")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != "FAILED" {
		t.Errorf("job status = %q, want FAILED", got.Status)
	}
	if got.Error != search.JobAttemptsExhausted() {
		t.Errorf("job error = %q, want %q", got.Error, search.JobAttemptsExhausted())
	}
}

// createRunningReclaimJobAt is createStaleReclaimJob with a caller-chosen
// CreateTime, so a test can construct a job that is NOT stale by heartbeat
// baseline (e.g. created "now") and make it claimable purely via some other
// mechanism (the store's Release mark) under test — isolating that mechanism
// from ordinary staleness.
func createRunningReclaimJobAt(t *testing.T, store spi.AsyncSearchStore, tenant spi.TenantID, id string, ref spi.ModelRef, cond predicate.Condition, pit, createTime time.Time) {
	t.Helper()
	condJSON, err := json.Marshal(cond)
	if err != nil {
		t.Fatalf("marshal condition: %v", err)
	}
	optsJSON, err := json.Marshal(struct {
		Limit       int             `json:"limit"`
		PointInTime *time.Time      `json:"pointInTime,omitempty"`
		OrderBy     []spi.OrderSpec `json:"orderBy,omitempty"`
	}{Limit: 10, PointInTime: &pit})
	if err != nil {
		t.Fatalf("marshal opts: %v", err)
	}
	job := &spi.SearchJob{
		ID:         id,
		TenantID:   tenant,
		Status:     "RUNNING",
		ModelRef:   ref,
		Condition:  condJSON,
		SearchOpts: optsJSON,
		CreateTime: createTime,
	}
	if err := store.CreateJob(tenantCtx(string(tenant)), job); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

// (c) a job that is released via the store's Release (a graceful handoff —
// not a lost executor) and then reclaimed completes with StaleClaims still
// 0: a released claim is never counted as an attempt, matching
// AsyncSearchStore.Release's contract. CreateTime is "now" so the ONLY reason
// this job is claimable at all is the Release mark, isolating that path from
// ordinary staleness.
func TestReclaimStaleJobs_ReleaseThenClaimDoesNotCount(t *testing.T) {
	svc, factory, store := newReclaimTestService(t)
	ctx := tenantCtx("tenant-a")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	now := time.Now()
	createRunningReclaimJobAt(t, store, "tenant-a", "job-released", ref, cond, now, now)

	if err := store.Release(ctx, "job-released", 1); err != nil {
		t.Fatalf("Release: %v", err)
	}

	reenq, failed, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if reenq != 1 || failed != 0 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, failed %d), want (1, 0) for a released (not stale) job", reenq, failed)
	}

	status := pollUntilTerminal(t, svc, ctx, "job-released", 5*time.Second)
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("reclaimed job status = %q, want SUCCESSFUL", status.Status)
	}

	got, err := store.GetJob(ctx, "job-released")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.StaleClaims != 0 {
		t.Errorf("StaleClaims after a release-then-claim reclaim = %d, want 0 (a graceful handoff must never count as an attempt)", got.StaleClaims)
	}
}

// claimStaleFailStore wraps a real spi.AsyncSearchStore and fails the test if
// ClaimStale is ever called on it, mirroring
// reaper_self_executing_test.go's selfExecutingAsyncStore pattern but for a
// single method. Used to enforce that the headroom<=0 short circuit actually
// skips the store call rather than merely happening to return an empty claim.
type claimStaleFailStore struct {
	spi.AsyncSearchStore
	t *testing.T
}

func (s *claimStaleFailStore) ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*spi.SearchJob, error) {
	s.t.Fatal("ClaimStale must never be called when the node has zero reclaim headroom")
	return nil, nil
}

// (d) a node whose pool capacity is fully consumed by its own registry
// (queued + executing jobs on this node) has zero headroom: ReclaimStaleJobs
// must return (0, 0, nil) WITHOUT ever calling the store's ClaimStale —
// claiming stale jobs this node has no room to start would just strand them
// RUNNING with no executor.
func TestReclaimStaleJobs_ZeroHeadroomClaimsNothing(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &claimStaleFailStore{AsyncSearchStore: base, t: t}

	pool := search.NewWorkerPool(2, 3) // Cap() == 5
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool)

	uc := &spi.UserContext{Tenant: spi.Tenant{ID: "tenant-headroom"}}
	for i := 0; i < pool.Cap(); i++ {
		if ok := svc.RegisterJobForTest(fmt.Sprintf("occupier-%d", i), func(error) {}, uc); !ok {
			t.Fatalf("RegisterJobForTest(occupier-%d) = false, want true", i)
		}
	}
	if got := svc.RegisteredJobCountForTest(); got != pool.Cap() {
		t.Fatalf("registry size = %d, want %d (== pool.Cap(), saturated)", got, pool.Cap())
	}

	reenq, failed, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if reenq != 0 || failed != 0 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, failed %d), want (0, 0) with zero headroom", reenq, failed)
	}
}

// clearResultsErrorStore wraps a real spi.AsyncSearchStore and makes
// ClearResults always fail, to drive ReclaimStaleJobs's release-on-error
// branch.
type clearResultsErrorStore struct {
	spi.AsyncSearchStore
}

func (s *clearResultsErrorStore) ClearResults(ctx context.Context, jobID string) error {
	return fmt.Errorf("clearResultsErrorStore: simulated ClearResults failure for %s", jobID)
}

// (e) a ClearResults failure during reclaim must leave the job RUNNING and
// RELEASED — never FAILED (the job itself may be fine; only its prior-epoch
// partial results couldn't be cleared) and never run over unknown residue
// (fail closed). The clear runs on the worker, under the job's heartbeat, so
// the sweep counts the job as re-enqueued and the release follows on the
// worker. "Released" is asserted indirectly: a
// subsequent ClaimStale with a long staleAfter re-takes the job immediately,
// which only a released (or genuinely stale) job would allow — and
// StaleClaims must not be bumped a second time by that re-take, proving the
// release itself (as opposed to the original staleness claim that brought
// the job in) was not counted as another attempt.
func TestReclaimStaleJobs_ClearResultsErrorReleases(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &clearResultsErrorStore{AsyncSearchStore: base}

	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	// Stale by heartbeat baseline (CreateTime an hour in the past) so the
	// FIRST ClaimStale below actually claims it in the ordinary way; the
	// point under test is what happens to it AFTER that claim.
	createStaleReclaimJob(t, store, "tenant-a", "job-clear-err", ref, cond, time.Now())

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool)

	reenq, failed, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if reenq != 1 || failed != 0 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, failed %d), want (1, 0): the clear runs on the worker", reenq, failed)
	}

	// The worker's release is what makes the job eligible again at once.
	var claimed []*spi.SearchJob
	deadline := time.Now().Add(2 * time.Second)
	for len(claimed) == 0 && time.Now().Before(deadline) {
		got, err := store.GetJob(tenantCtx("tenant-a"), "job-clear-err")
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.Status != "RUNNING" {
			t.Fatalf("job status after a ClearResults error = %q, want RUNNING (never FAILED, never silently dropped)", got.Status)
		}
		if claimed, err = base.ClaimStale(context.Background(), time.Hour, 10); err != nil {
			t.Fatalf("ClaimStale: %v", err)
		}
		if len(claimed) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if len(claimed) != 1 || claimed[0].ID != "job-clear-err" {
		t.Fatalf("ClaimStale(staleAfter=1h) after a ClearResults-error release = %v, want [job-clear-err] (a released job is eligible regardless of staleAfter)", claimed)
	}
	if claimed[0].StaleClaims != 1 {
		t.Errorf("StaleClaims after the release-triggering ClearResults error = %d, want 1 (only the original staleness claim that brought the job in counts; the ClearResults-error release itself must not add another)", claimed[0].StaleClaims)
	}
}

// (f) self-reclaim: this node re-registers a jobID it already has a handle
// for (its own paused/stuck executor from a prior claim). registerReclaim
// must cancel the OLD handle's context with errJobSuperseded and install the
// new handle in its place; a deregistration presented with the OLD handle
// identity must be a no-op against the new registration (the compare-and-
// delete guard deregisterJobHandle relies on to keep a superseded executor's
// deferred cleanup from evicting the handle that replaced it).
func TestReclaimStaleJobs_SelfReclaimReplacesHandle(t *testing.T) {
	svc, _, _ := newReclaimTestService(t)
	uc := &spi.UserContext{Tenant: spi.Tenant{ID: "tenant-self-reclaim"}}

	oldCtx, oldCancel := context.WithCancelCause(context.Background())
	oldHandle := svc.RegisterJobHandleForTest("job-self", oldCancel, uc)
	if oldHandle == nil {
		t.Fatal("RegisterJobHandleForTest returned a nil handle")
	}

	newCancelCalled := false
	newCancel := func(error) { newCancelCalled = true }
	newHandle := svc.RegisterReclaimForTest("job-self", newCancel, uc, 2)
	if newHandle == nil {
		t.Fatal("RegisterReclaimForTest returned a nil handle")
	}

	select {
	case <-oldCtx.Done():
	default:
		t.Fatal("self-reclaim must cancel the superseded handle's context synchronously")
	}
	if cause := context.Cause(oldCtx); !errors.Is(cause, search.ErrJobSuperseded()) {
		t.Fatalf("context.Cause(oldCtx) = %v, want errJobSuperseded", cause)
	}

	// The registry still holds exactly one entry for "job-self" — the NEW
	// handle. Deregistering by the OLD handle's identity must be a no-op.
	if got := svc.RegisteredJobCountForTest(); got != 1 {
		t.Fatalf("registry size after self-reclaim = %d, want 1 (one entry, now the new handle)", got)
	}
	svc.DeregisterJobHandleForTest("job-self", oldHandle)
	if got := svc.RegisteredJobCountForTest(); got != 1 {
		t.Fatalf("registry size after deregistering the SUPERSEDED old handle = %d, want 1 (must not evict the new epoch's handle)", got)
	}
	if newCancelCalled {
		t.Error("deregistering the old handle must not invoke the new handle's cancel func")
	}

	// The new handle is what is actually installed: deregistering IT clears
	// the entry.
	svc.DeregisterJobHandleForTest("job-self", newHandle)
	if got := svc.RegisteredJobCountForTest(); got != 0 {
		t.Fatalf("registry size after deregistering the new (current) handle = %d, want 0", got)
	}
}
