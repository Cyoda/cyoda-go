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

func (s *mainPoolBlockedStore) ClearResults(ctx context.Context, jobID string) error {
	if err := s.mainPool(ctx); err != nil {
		return err
	}
	return s.AsyncSearchStore.ClearResults(ctx, jobID)
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
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(20 * time.Millisecond)
	t.Cleanup(store.release) // runs before the pool drains

	store.blocked.Store(true)
	type sweep struct {
		reenqueued, failed int
		err                error
	}
	done := make(chan sweep, 1)
	go func() {
		r, f, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5)
		done <- sweep{r, f, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.reenqueued != 1 || got.failed != 0 {
			t.Fatalf("ReclaimStaleJobs = (reenqueued %d, failed %d, err %v), want (1, 0, nil)", got.reenqueued, got.failed, got.err)
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
