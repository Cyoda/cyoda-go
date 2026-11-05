package search_test

// What a reclaimed job does when it does not run: which store statements its
// worker sends after a refused clear, and after its context ended before the
// clear.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
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
			logs := captureJobLogs(t)
			clock := memory.NewTestClock()
			factory := memory.NewStoreFactory(memory.WithClock(clock))
			t.Cleanup(func() { factory.Close() })
			base, err := factory.AsyncSearchStore(context.Background())
			if err != nil {
				t.Fatalf("AsyncSearchStore: %v", err)
			}
			store := &clearOutcomeStore{AsyncSearchStore: base, refuse: refusal}
			ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
			cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
			createStaleReclaimJob(t, clock, base, "tenant-a", "job-refused", ref, cond, time.Now())

			pool := search.NewWorkerPool(1, 4)
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
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
			assertOneLostClaimLine(t, logs.String(), "job-refused")
		})
	}
}

// captureJobLogs routes the default logger, at DEBUG, into a buffer for the
// test's duration.
func captureJobLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// assertOneLostClaimLine requires that the only line logged for jobID among
// out's lines is the one lost-claim report, at INFO.
func assertOneLostClaimLine(t *testing.T, out, jobID string) {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "jobID="+jobID) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], lostClaimLine) || !strings.Contains(lines[0], "level=INFO") {
		t.Fatalf("lines for %s = %q, want one lost-claim line at INFO", jobID, lines)
	}
}

// A reclaimed job cancelled on this node while it waits in the queue ends
// before its clear with one lost-claim line, and sends no Release: the
// cancel already settled it.
func TestReclaimStaleJobs_CancelledBeforeWorkerRunsEndsAsALostClaim(t *testing.T) {
	clock := memory.NewTestClock()
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &clearOutcomeStore{AsyncSearchStore: base}
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, clock, base, "tenant-a", "job-cancel-queued", ref, cond, time.Now())

	pool := search.NewWorkerPool(1, 4)
	hold := make(chan struct{})
	if err := pool.Submit(func() { <-hold }); err != nil {
		t.Fatalf("Submit the holder: %v", err)
	}
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)
	if reenq, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 5); err != nil || reenq != 1 {
		t.Fatalf("ReclaimStaleJobs = (reenqueued %d, err %v), want (1, nil)", reenq, err)
	}

	logs := captureJobLogs(t)
	if _, err := svc.CancelAsync(tenantCtx("tenant-a"), "job-cancel-queued"); err != nil {
		t.Fatalf("CancelAsync: %v", err)
	}
	close(hold)
	pool.Drain(context.Background())

	if got := store.clears.Load(); got != 0 {
		t.Errorf("ClearResults calls = %d, want 0", got)
	}
	if got := store.releases.Load(); got != 0 {
		t.Errorf("Release calls = %d, want 0: the cancel settled the job", got)
	}
	assertOneLostClaimLine(t, logs.String(), "job-cancel-queued")
}

// A reclaimed job released at shutdown while it waits in the queue ends
// before its clear. Its worker sends no clear and no second Release: the
// shutdown release already handed the job back, uncounted.
func TestReclaimStaleJobs_ReleasedBeforeWorkerRunsSendsNoClear(t *testing.T) {
	clock := memory.NewTestClock()
	factory := memory.NewStoreFactory(memory.WithClock(clock))
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &clearOutcomeStore{AsyncSearchStore: base}
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, clock, base, "tenant-a", "job-queued", ref, cond, time.Now())

	// One worker, held busy, so the reclaimed job waits in the queue.
	pool := search.NewWorkerPool(1, 4)
	hold := make(chan struct{})
	if err := pool.Submit(func() { <-hold }); err != nil {
		t.Fatalf("Submit the holder: %v", err)
	}
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real()).
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
