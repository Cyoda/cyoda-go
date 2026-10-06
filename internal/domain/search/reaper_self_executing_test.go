package search_test

// TestReclaimStaleJobs_SelfExecutingStore_NeverClaimsOrWrites pins that the
// stale-job reclaim sweep keeps the self-executing-store guard SubmitAsync
// already has. A self-executing store's jobs never receive an engine
// heartbeat (SubmitAsync skips its background goroutine for these stores), so
// ClaimStale's COALESCE(heartbeat_time, created_at) baseline would make every
// healthy job of theirs look stale after staleAfter by construction — and,
// absent this guard, ReclaimStaleJobs would claim them and clear/re-run or
// FAIL them out from under the store's own recovery pipeline. Fail closed:
// skip reclaim entirely for a spi.SelfExecutingSearchStore, exactly as
// SubmitAsync skips its own background execution for one.

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// selfExecutingAsyncStore wraps a real spi.AsyncSearchStore, implements
// spi.SelfExecutingSearchStore, and fails the test if ClaimStale or
// UpdateJobStatus is ever called on it — the two calls SubmitAsync's own
// guard's doc comment says are an error against a self-executing store.
type selfExecutingAsyncStore struct {
	spi.AsyncSearchStore
	t *testing.T
}

func (s *selfExecutingAsyncStore) SelfExecuting() {}

func (s *selfExecutingAsyncStore) ClaimStale(ctx context.Context, staleAfter time.Duration, batch int) ([]*spi.SearchJob, error) {
	s.t.Fatal("ClaimStale must never be called on a self-executing store by the reaper")
	return nil, nil
}

func (s *selfExecutingAsyncStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	s.t.Fatal("UpdateJobStatus must never be called on a self-executing store by the reaper")
	return nil
}

var _ spi.SelfExecutingSearchStore = (*selfExecutingAsyncStore)(nil)

func TestReclaimStaleJobs_SelfExecutingStore_NeverClaimsOrWrites(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &selfExecutingAsyncStore{AsyncSearchStore: base, t: t}

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory)).
		WithAsyncPool(pool)

	reenq, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, search.StaleClaimBatch)
	if err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	if reenq != 0 {
		t.Fatalf("ReclaimStaleJobs re-enqueued %d, want 0 for a self-executing store", reenq)
	}
}
