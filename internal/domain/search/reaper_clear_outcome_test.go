package search_test

// What a reclaimed job does when it does not run: which store statements its
// worker sends after a refused clear, and after its context ended before the
// clear.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// clearOutcomeStore counts ClearResults and Release calls. When refuse is set,
// every ClearResults answers it and deletes nothing.
type clearOutcomeStore struct {
	spi.AsyncSearchStore
	refuse   error
	clears   atomic.Int32
	releases atomic.Int32
}

func (s *clearOutcomeStore) ClearResults(ctx context.Context, jobID string, epoch int64) error {
	s.clears.Add(1)
	if s.refuse != nil {
		return fmt.Errorf("clear of %s at epoch %d: %w", jobID, epoch, s.refuse)
	}
	return s.AsyncSearchStore.ClearResults(ctx, jobID, epoch)
}

func (s *clearOutcomeStore) Release(ctx context.Context, jobID string, epoch int64) error {
	s.releases.Add(1)
	return s.AsyncSearchStore.Release(ctx, jobID, epoch)
}

// A clear refused because the job was taken, settled or is gone leaves
// nothing for this node to hand back: the worker sends no Release.
func TestReclaimStaleJobs_RefusedClearSendsNoRelease(t *testing.T) {
	for _, refusal := range []error{spi.ErrStaleClaim, spi.ErrAlreadyTerminal, spi.ErrNotFound} {
		t.Run(refusal.Error(), func(t *testing.T) {
			factory := memory.NewStoreFactory()
			t.Cleanup(func() { factory.Close() })
			base, err := factory.AsyncSearchStore(context.Background())
			if err != nil {
				t.Fatalf("AsyncSearchStore: %v", err)
			}
			store := &clearOutcomeStore{AsyncSearchStore: base, refuse: refusal}
			ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
			cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
			createStaleReclaimJob(t, base, "tenant-a", "job-refused", ref, cond, time.Now())

			pool := search.NewWorkerPool(1, 4)
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
				WithAsyncPool(pool).
				WithHeartbeat(time.Hour)

			if reenq, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5); err != nil || reenq != 1 {
				t.Fatalf("ReclaimStaleJobs = (reenqueued %d, err %v), want (1, nil)", reenq, err)
			}
			pool.Drain(context.Background()) // the worker has finished

			if got := store.clears.Load(); got != 1 {
				t.Fatalf("ClearResults calls = %d, want 1", got)
			}
			if got := store.releases.Load(); got != 0 {
				t.Errorf("Release calls after a clear refused with %v = %d, want 0", refusal, got)
			}
			if got := svc.RegisteredJobCountForTest(); got != 0 {
				t.Errorf("registry size = %d, want 0: the job that did not run is deregistered", got)
			}
		})
	}
}

// A reclaimed job released at shutdown while it waits in the queue ends
// before its clear. Its worker sends no clear and no second Release: the
// shutdown release already handed the job back, uncounted.
func TestReclaimStaleJobs_ReleasedBeforeWorkerRunsSendsNoClear(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &clearOutcomeStore{AsyncSearchStore: base}
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, base, "tenant-a", "job-queued", ref, cond, time.Now())

	// One worker, held busy, so the reclaimed job waits in the queue.
	pool := search.NewWorkerPool(1, 4)
	hold := make(chan struct{})
	if err := pool.Submit(func() { <-hold }); err != nil {
		t.Fatalf("Submit the holder: %v", err)
	}
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)

	if reenq, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5); err != nil || reenq != 1 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, err %v), want (1, nil)", reenq, err)
	}
	if n := svc.ReleaseRegisteredJobs(context.Background()); n != 1 {
		t.Fatalf("ReleaseRegisteredJobs = %d, want 1", n)
	}
	close(hold)
	pool.Drain(context.Background()) // the reclaimed job's worker has finished

	if got := store.clears.Load(); got != 0 {
		t.Errorf("ClearResults calls = %d, want 0: the job's context ended before its worker ran", got)
	}
	if got := store.releases.Load(); got != 1 {
		t.Errorf("Release calls = %d, want 1 (the shutdown release only)", got)
	}
	if got := svc.RegisteredJobCountForTest(); got != 0 {
		t.Errorf("registry size = %d, want 0", got)
	}
	claimed, err := base.ClaimStale(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "job-queued" {
		t.Fatalf("ClaimStale(staleAfter=1h) = %v, want [job-queued]: the released job is claimable at once", claimed)
	}
	if claimed[0].StaleClaims != 1 {
		t.Errorf("StaleClaims = %d, want 1: the release is not counted", claimed[0].StaleClaims)
	}
	if got, err := base.GetJob(tenantCtx("tenant-a"), "job-queued"); err != nil || got.Status != "RUNNING" {
		t.Fatalf("GetJob = (%v, %v), want RUNNING", got, err)
	}
}
