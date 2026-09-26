package search_test

// Coverage for the startHeartbeat busy-tick policy: a Heartbeat tick that
// meets a lock its own job's SaveResults holds is a distinct, transient
// outcome (spi.ErrTaskBusy) — startHeartbeat must treat it as a missed tick
// and retry on the next one, not abort a healthy job. Any other error that
// is not a fencing refusal (a dropped connection, a timeout) is a missed tick
// too. Only a fencing refusal — stale claim, terminal, not found — aborts it.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// slowIterator sleeps briefly before each item is produced, stretching
// runAsyncJob's scan across several heartbeat ticks so a busy tick's effect
// (or lack of one) is observable before the job finishes.
type slowIterator struct {
	spi.Iterator
	perItem time.Duration
}

func (s *slowIterator) Next() bool {
	time.Sleep(s.perItem)
	return s.Iterator.Next()
}

// scriptedHeartbeatStore serves a real store's Heartbeat except on the calls
// named in busyOnCalls, transientOnCalls or staleOnCalls (1-indexed), which
// answer a marked-busy, a plain transport or a stale-claim error instead
// without touching the store.
type scriptedHeartbeatStore struct {
	spi.AsyncSearchStore
	busyOnCalls      map[int]bool
	transientOnCalls map[int]bool
	staleOnCalls     map[int]bool

	calls int32
}

func (s *scriptedHeartbeatStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	n := int(atomic.AddInt32(&s.calls, 1))
	if s.busyOnCalls[n] {
		return fmt.Errorf("heartbeat search job %s: %w: row locked", jobID, spi.ErrTaskBusy)
	}
	if s.transientOnCalls[n] {
		return fmt.Errorf("heartbeat search job %s: read tcp 10.0.0.1:5432: connection reset by peer", jobID)
	}
	if s.staleOnCalls[n] {
		return fmt.Errorf("heartbeat search job %s: %w", jobID, spi.ErrStaleClaim)
	}
	return s.AsyncSearchStore.Heartbeat(ctx, jobID, epoch)
}

func (s *scriptedHeartbeatStore) callCount() int {
	return int(atomic.LoadInt32(&s.calls))
}

// newSlowSearchService builds a service over a real memory store whose
// entity scan is stretched with slowIterator, and whose AsyncSearchStore is
// wrapped by store — so the test controls what individual Heartbeat calls
// answer while everything else (CreateJob, SaveResults, UpdateJobStatus,
// GetJob) runs for real.
func newSlowSearchService(t *testing.T, ctx context.Context, base *memory.StoreFactory, store spi.AsyncSearchStore, perItem, heartbeatEvery time.Duration) (*search.SearchService, *iterableEntityStore) {
	t.Helper()
	realEntityStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ies := &iterableEntityStore{
		EntityStore: realEntityStore,
		iterateFn: func(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
			it, err := realEntityStore.Iterate(ctx, model, filter, opts)
			if err != nil {
				return nil, err
			}
			return &slowIterator{Iterator: it, perItem: perItem}, nil
		},
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(heartbeatEvery)
	return svc, ies
}

// submitSlowJob seeds n trivial entities and submits an async search over
// all of them, so the scan takes roughly n*perItem to complete.
func submitSlowJob(t *testing.T, ctx context.Context, base *memory.StoreFactory, svc *search.SearchService, n int) string {
	t.Helper()
	ref := spi.ModelRef{EntityName: "slowitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < n; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%05d", i), []byte(`{}`))
	}
	jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{Limit: n})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	return jobID
}

// A busy tick must not cancel a healthy job: the ticker treats it as a
// missed heartbeat and keeps trying on the next tick, and the scan runs to
// completion.
func TestStartHeartbeat_BusyTickDoesNotCancelTheJob(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-heartbeat-busy")

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &scriptedHeartbeatStore{
		AsyncSearchStore: realAsync,
		busyOnCalls:      map[int]bool{1: true, 2: true},
	}
	svc, _ := newSlowSearchService(t, ctx, base, store, 15*time.Millisecond, 20*time.Millisecond)

	jobID := submitSlowJob(t, ctx, base, svc, 30)
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)

	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL — a busy heartbeat tick aborted a healthy job", status.Status)
	}
	if got := store.callCount(); got < 3 {
		t.Fatalf("Heartbeat was called %d times, want at least 3 — the ticker must keep retrying past the busy ticks, not stop after them", got)
	}
}

// An error that is not a fencing refusal must not cancel a healthy job
// either: the claim is not lost, every later write of the job is still
// epoch-fenced, and if the stamps keep failing another node takes the job
// over once the store's staleness window passes.
func TestStartHeartbeat_TransientErrorDoesNotCancelTheJob(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-heartbeat-transient")

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &scriptedHeartbeatStore{
		AsyncSearchStore: realAsync,
		transientOnCalls: map[int]bool{1: true, 2: true},
	}
	svc, _ := newSlowSearchService(t, ctx, base, store, 15*time.Millisecond, 20*time.Millisecond)

	jobID := submitSlowJob(t, ctx, base, svc, 30)
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)

	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL — a transient heartbeat error aborted a healthy job", status.Status)
	}
	if got := store.callCount(); got < 3 {
		t.Fatalf("Heartbeat was called %d times, want at least 3 — the ticker must keep stamping past the failed ticks", got)
	}
}

// A stale-claim answer is a genuine fencing refusal, not a missed tick, and
// must still abort the job exactly as before this fix.
func TestStartHeartbeat_StaleClaimStillAbortsTheJob(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-heartbeat-stale")

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &scriptedHeartbeatStore{
		AsyncSearchStore: realAsync,
		staleOnCalls:     map[int]bool{1: true},
	}
	svc, _ := newSlowSearchService(t, ctx, base, store, 15*time.Millisecond, 20*time.Millisecond)

	jobID := submitSlowJob(t, ctx, base, svc, 30)
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)

	if status.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED — a stale-claim heartbeat answer must still abort the job", status.Status)
	}
}
