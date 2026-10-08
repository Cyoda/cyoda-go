package search_test

// The async-search heartbeat and the reclaim sweep must not wait on the
// store's main pool. A backend such as PostgreSQL runs Heartbeat and
// ClaimStale on a pool of their own; every other job statement shares the
// pool entity transactions use. mainPoolBlockedStore models that pool being
// exhausted: once blocked, every statement except Heartbeat and ClaimStale
// waits until the test releases it.

import (
	"context"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type mainPoolBlockedStore struct {
	spi.AsyncSearchStore

	blocked    atomic.Bool
	unblock    chan struct{}
	unblockOne sync.Once
	heartbeats atomic.Int32
}

func newMainPoolBlockedStore(base spi.AsyncSearchStore) *mainPoolBlockedStore {
	return &mainPoolBlockedStore{AsyncSearchStore: base, unblock: make(chan struct{})}
}

// release frees the main pool for good. Safe to call more than once.
func (s *mainPoolBlockedStore) release() { s.unblockOne.Do(func() { close(s.unblock) }) }

// mainPool waits, while the pool is blocked, until the test releases it.
func (s *mainPoolBlockedStore) mainPool(ctx context.Context) error {
	if !s.blocked.Load() {
		return nil
	}
	select {
	case <-s.unblock:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *mainPoolBlockedStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	s.heartbeats.Add(1)
	return s.AsyncSearchStore.Heartbeat(ctx, jobID, epoch)
}

func (s *mainPoolBlockedStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.CreateJob(ctx, job)
}

func (s *mainPoolBlockedStore) GetJob(ctx context.Context, jobID string) (*spi.SearchJob, error) {
	if err := s.mainPool(ctx); err != nil {
		return nil, err
	}
	return s.AsyncSearchStore.GetJob(ctx, jobID)
}

func (s *mainPoolBlockedStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

func (s *mainPoolBlockedStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.SaveResults(ctx, jobID, epoch, entityIDs)
}

func (s *mainPoolBlockedStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	if err := s.mainPool(ctx); err != nil {
		return nil, 0, err
	}
	return s.AsyncSearchStore.GetResultIDs(ctx, jobID, offset, limit)
}

func (s *mainPoolBlockedStore) DeleteJob(ctx context.Context, jobID string) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.DeleteJob(ctx, jobID)
}

func (s *mainPoolBlockedStore) ReapExpired(ctx context.Context, ttl time.Duration) (int, error) {
	if err := s.mainPool(ctx); err != nil {
		return 0, err
	}
	return s.AsyncSearchStore.ReapExpired(ctx, ttl)
}

func (s *mainPoolBlockedStore) Cancel(ctx context.Context, jobID string, finishTime time.Time) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.Cancel(ctx, jobID, finishTime)
}

func (s *mainPoolBlockedStore) ClearResults(ctx context.Context, jobID string, epoch int64) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.ClearResults(ctx, jobID, epoch)
}

func (s *mainPoolBlockedStore) Release(ctx context.Context, jobID string, epoch int64) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.Release(ctx, jobID, epoch)
}

// awaitHeartbeats waits until the store has seen at least want Heartbeat calls.
func awaitHeartbeats(t *testing.T, store *mainPoolBlockedStore, want int32, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if store.heartbeats.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: %d heartbeats within %s, want at least %d", what, store.heartbeats.Load(), within, want)
}

// A reclaimed job is heartbeated, and the reclaim sweep returns, while the main
// pool is exhausted: the job's ClearResults runs under its heartbeat, not ahead
// of it in the sweep.
func TestReclaimStaleJobs_MainPoolExhausted_SweepReturnsAndJobIsHeartbeated(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := newMainPoolBlockedStore(base)

	ctx := tenantCtx("tenant-a")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, base, "tenant-a", "job-starved", ref, cond, time.Now())

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory)).
		WithAsyncPool(pool).
		WithHeartbeat(20 * time.Millisecond)
	t.Cleanup(store.release) // runs before the pool drains

	store.blocked.Store(true)
	type sweep struct {
		reenqueued int
		err        error
	}
	done := make(chan sweep, 1)
	go func() {
		r, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5)
		done <- sweep{r, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.reenqueued != 1 {
			t.Fatalf("ReclaimStaleJobs = (reenqueued %d, err %v), want (1, nil)", got.reenqueued, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReclaimStaleJobs did not return within 2s with the main pool exhausted: the sweep waits on a main-pool statement")
	}

	awaitHeartbeats(t, store, 5, 2*time.Second, "the reclaimed job's heartbeat with the main pool exhausted")

	store.release()
	if status := pollUntilTerminal(t, svc, ctx, "job-starved", 5*time.Second); status.Status != "SUCCESSFUL" {
		t.Fatalf("reclaimed job status = %q, want SUCCESSFUL", status.Status)
	}
	ids, total, err := store.GetResultIDs(ctx, "job-starved", 0, 10)
	if err != nil {
		t.Fatalf("GetResultIDs: %v", err)
	}
	if total != 1 || len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("results = %v (total %d), want [e1]", ids, total)
	}
}

// A running job keeps heartbeating while the main pool is exhausted: the
// heartbeat loop issues no main-pool statement of its own.
func TestStartHeartbeat_MainPoolExhausted_KeepsStamping(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-heartbeat-pool")

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := newMainPoolBlockedStore(realAsync)
	svc, _ := newSlowSearchService(t, ctx, base, store, 15*time.Millisecond, 20*time.Millisecond)
	t.Cleanup(store.release) // runs before the pool drains

	jobID := submitSlowJob(t, ctx, base, svc, 30)
	awaitHeartbeats(t, store, 1, 2*time.Second, "the first heartbeat")

	store.blocked.Store(true)
	before := store.heartbeats.Load()
	awaitHeartbeats(t, store, before+5, 2*time.Second, "heartbeats with the main pool exhausted")

	store.release()
	if status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second); status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
	}
}

// A cancel written by another node (the store's Cancel, with no in-process
// CancelRunning) stops this node's executor: its next Heartbeat is refused as
// already terminal, which ends the job's context and the heartbeat with it.
func TestStartHeartbeat_CancelFromAnotherNodeStopsTheHeartbeat(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-heartbeat-cancel")

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := newMainPoolBlockedStore(realAsync)
	svc, _ := newSlowSearchService(t, ctx, base, store, 30*time.Millisecond, 20*time.Millisecond)

	jobID := submitSlowJob(t, ctx, base, svc, 100)
	awaitHeartbeats(t, store, 2, 2*time.Second, "the job's heartbeat")

	if err := realAsync.Cancel(ctx, jobID, time.Now()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// The refusal lands within a tick; after that the count must stay still.
	time.Sleep(200 * time.Millisecond)
	settled := store.heartbeats.Load()
	time.Sleep(200 * time.Millisecond)
	if got := store.heartbeats.Load(); got != settled {
		t.Fatalf("heartbeats went on after another node's cancel: %d then %d", settled, got)
	}
	if status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second); status.Status != "CANCELLED" {
		t.Fatalf("status = %q, want CANCELLED", status.Status)
	}
}

// orderedClaimStore answers ClaimStale in job-ID order, so a test fixes which
// job of a batch the sweep meets first. It hands back the job named
// undecodable with search options that do not decode, and counts the
// Heartbeat calls for the job named watch.
type orderedClaimStore struct {
	*mainPoolBlockedStore
	undecodable string
	watch       string
	watchBeats  atomic.Int32
}

func (s *orderedClaimStore) ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*spi.SearchJob, error) {
	jobs, err := s.mainPoolBlockedStore.ClaimStale(ctx, staleAfter, limit)
	slices.SortFunc(jobs, func(a, b *spi.SearchJob) int { return strings.Compare(a.ID, b.ID) })
	for _, j := range jobs {
		if j.ID == s.undecodable {
			j.SearchOpts = []byte("not json")
		}
	}
	return jobs, err
}

func (s *orderedClaimStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	if jobID == s.watch {
		s.watchBeats.Add(1)
	}
	return s.mainPoolBlockedStore.Heartbeat(ctx, jobID, epoch)
}

// The sweep starts every job it can run before it writes anything for a job it
// will not run. Here a capped job and an undecodable one come first in the
// batch, and their FAILED writes wait on an exhausted main pool; the runnable
// job after them is heartbeated all the same.
func TestReclaimStaleJobs_BlockedWriteForUnrunJob_DoesNotDelayRunnableJob(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &orderedClaimStore{mainPoolBlockedStore: newMainPoolBlockedStore(base), watch: "c-runnable"}

	ctx := tenantCtx("tenant-a")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}

	// Stale: the claim counts, and with maxAttempts 1 the job is at its cap.
	createStaleReclaimJob(t, base, "tenant-a", "a-capped", ref, cond, time.Now())
	// Released, so the claim does not count: an undecodable job, and a
	// runnable one.
	createRunningReclaimJobAt(t, base, "tenant-a", "b-undecodable", ref, cond, time.Now(), time.Now())
	createRunningReclaimJobAt(t, base, "tenant-a", "c-runnable", ref, cond, time.Now(), time.Now())
	for _, id := range []string{"b-undecodable", "c-runnable"} {
		if err := base.Release(ctx, id, 1); err != nil {
			t.Fatalf("Release(%s): %v", id, err)
		}
	}
	store.undecodable = "b-undecodable"

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory)).
		WithAsyncPool(pool).
		WithHeartbeat(20 * time.Millisecond)
	t.Cleanup(store.release) // runs before the pool drains

	store.blocked.Store(true)
	type sweep struct {
		reenqueued int
		err        error
	}
	done := make(chan sweep, 1)
	go func() {
		r, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 1)
		done <- sweep{r, err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for store.watchBeats.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := store.watchBeats.Load(); got < 3 {
		t.Fatalf("the runnable job got %d heartbeats within 2s, want at least 3: a blocked write for a job the sweep will not run delayed it", got)
	}

	store.release()
	select {
	case got := <-done:
		if got.err != nil || got.reenqueued != 1 {
			t.Fatalf("ReclaimStaleJobs = (reenqueued %d, err %v), want (1, nil)", got.reenqueued, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReclaimStaleJobs did not return after the main pool was released")
	}
	for id, want := range map[string]string{"a-capped": "FAILED", "b-undecodable": "FAILED", "c-runnable": "SUCCESSFUL"} {
		if status := pollUntilTerminal(t, svc, ctx, id, 5*time.Second); status.Status != want {
			t.Errorf("%s status = %q, want %s", id, status.Status, want)
		}
	}
}

// secondPassStore counts the second-pass FAILED writes waiting on the main
// pool right now.
type secondPassStore struct {
	*orderedClaimStore
	waiting atomic.Int32
}

func (s *secondPassStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if status == "FAILED" {
		s.waiting.Add(1)
		defer s.waiting.Add(-1)
	}
	return s.orderedClaimStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

// awaitCount waits until get() reaches want.
func awaitCount(t *testing.T, get func() int32, want int32, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if get() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: %d within %s, want at least %d", what, get(), within, want)
}

// The claim loop never waits on the main pool. A sweep's FAILED write for a
// capped job blocks on an exhausted pool; the sweep returns all the same, and
// a later sweep still claims a newly claimable job and starts its heartbeat.
// The later sweep's own owed write joins the one second pass in flight rather
// than starting another, and nothing owed is lost once the pool frees.
func TestReclaimStaleJobs_BlockedSecondPass_LaterSweepStillClaims(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &secondPassStore{orderedClaimStore: &orderedClaimStore{mainPoolBlockedStore: newMainPoolBlockedStore(base), watch: "c-runnable"}}

	ctx := tenantCtx("tenant-a")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory)).
		WithAsyncPool(pool).
		WithHeartbeat(20 * time.Millisecond)
	t.Cleanup(store.release) // runs before the pool drains

	sweep := func(what string) int {
		t.Helper()
		type result struct {
			reenqueued int
			err        error
		}
		done := make(chan result, 1)
		go func() {
			r, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 1)
			done <- result{r, err}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("%s: ReclaimStaleJobs: %v", what, got.err)
			}
			return got.reenqueued
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not return within 2s with a second-pass write blocked on the main pool", what)
			return 0
		}
	}

	// Stale, so the claim counts: with maxAttempts 1 the job is at its cap.
	createStaleReclaimJob(t, base, "tenant-a", "a-capped", ref, cond, time.Now())
	store.blocked.Store(true)
	if got := sweep("the first sweep"); got != 0 {
		t.Fatalf("the first sweep re-enqueued %d, want 0", got)
	}
	awaitCount(t, store.waiting.Load, 1, 2*time.Second, "the capped job's FAILED write waiting on the main pool")

	createStaleReclaimJob(t, base, "tenant-a", "b-capped", ref, cond, time.Now())
	createRunningReclaimJobAt(t, base, "tenant-a", "c-runnable", ref, cond, time.Now(), time.Now())
	if err := base.Release(ctx, "c-runnable", 1); err != nil {
		t.Fatalf("Release(c-runnable): %v", err)
	}
	if got := sweep("the later sweep"); got != 1 {
		t.Fatalf("the later sweep re-enqueued %d, want 1 (c-runnable)", got)
	}
	awaitCount(t, store.watchBeats.Load, 3, 2*time.Second, "heartbeats of the job the later sweep claimed")

	// One second pass per node: the later sweep's owed write waits its turn
	// in the pass already in flight.
	time.Sleep(100 * time.Millisecond)
	if got := store.waiting.Load(); got != 1 {
		t.Fatalf("second-pass writes waiting on the main pool = %d, want 1: more than one second pass is in flight", got)
	}

	store.release()
	for id, want := range map[string]string{"a-capped": "FAILED", "b-capped": "FAILED", "c-runnable": "SUCCESSFUL"} {
		if status := pollUntilTerminal(t, svc, ctx, id, 5*time.Second); status.Status != want {
			t.Errorf("%s status = %q, want %s", id, status.Status, want)
		}
	}
}
