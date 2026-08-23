package search_test

// TestFailStaleJobs_SelfExecutingStore_NeverClaimsOrWrites pins the final-
// review finding that the stale-job reaper lacked the self-executing-store
// guard SubmitAsync already has. A self-executing store's jobs never
// receive an engine heartbeat (SubmitAsync skips its background goroutine
// for these stores), so ClaimStale's COALESCE(heartbeat_time, created_at)
// baseline would make every healthy job of theirs look stale after
// staleAfter by construction — and, absent this guard, FailStaleJobs would
// FAIL them out from under the store's own recovery pipeline. Fail closed:
// skip reaping entirely for a spi.SelfExecutingSearchStore, exactly as
// SubmitAsync skips its own background execution for one.

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
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

func TestFailStaleJobs_SelfExecutingStore_NeverClaimsOrWrites(t *testing.T) {
	base, _ := newReaperTestStore(t)
	store := &selfExecutingAsyncStore{AsyncSearchStore: base, t: t}

	n, err := search.FailStaleJobs(context.Background(), store, 5*time.Minute, search.StaleClaimBatch)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("FailStaleJobs claimed+failed %d jobs, want 0 (self-executing store, reaping must be skipped)", n)
	}
}
