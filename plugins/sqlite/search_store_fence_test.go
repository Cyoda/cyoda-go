package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// emptyIDs is an iter.Seq[string] that yields nothing — the ordinary outcome
// of a search matching zero rows.
func emptyIDs(func(string) bool) {}

// newSearchFenceStore builds a factory with one RUNNING job at epoch 1.
func newSearchFenceStore(t *testing.T, name string) (spi.AsyncSearchStore, context.Context, string) {
	t.Helper()
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-fence")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	job := &spi.SearchJob{
		ID:         "job-fence",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
		CreateTime: time.Now(),
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return store, ctx, job.ID
}

// TestSaveResults_EmptySequenceStillFencesOnEpoch: a search matching zero rows
// is an ordinary outcome, so SaveResults is routinely called with an empty
// sequence. The epoch fence must still run — otherwise an executor whose claim
// was already reclaimed by ClaimStale is told its (empty) write succeeded.
// The memory backend fences up front for exactly this reason; sqlite skipped
// the whole database round-trip when the batch was empty, so the two backends
// answered differently for the same call.
func TestSaveResults_EmptySequenceStillFencesOnEpoch(t *testing.T) {
	store, ctx, jobID := newSearchFenceStore(t, "fence_epoch.db")

	// Negative staleAfter pushes the cutoff into the future so the
	// just-created job qualifies without waiting or faking a clock.
	claimed, err := store.ClaimStale(ctx, -time.Hour, 10)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimStale: want 1 claimed job, got %d", len(claimed))
	}
	if claimed[0].Epoch != 2 {
		t.Fatalf("ClaimStale: want epoch 2 after claim, got %d", claimed[0].Epoch)
	}

	// Epoch 1 is now stale: the reclaim bumped it to 2.
	err = store.SaveResults(ctx, jobID, 1, iter.Seq[string](emptyIDs))
	if !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("SaveResults(empty, stale epoch) = %v, want ErrStaleClaim", err)
	}
}

// TestSaveResults_EmptySequenceStillFencesOnTerminal: terminal statuses are
// write-once, so a SaveResults against a job that already finished must be
// refused even when there is nothing to write.
func TestSaveResults_EmptySequenceStillFencesOnTerminal(t *testing.T) {
	store, ctx, jobID := newSearchFenceStore(t, "fence_terminal.db")

	if err := store.UpdateJobStatus(ctx, jobID, 1, "SUCCESSFUL", 0, "", time.Now(), 5); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	err := store.SaveResults(ctx, jobID, 1, iter.Seq[string](emptyIDs))
	if !errors.Is(err, spi.ErrAlreadyTerminal) {
		t.Fatalf("SaveResults(empty, terminal job) = %v, want ErrAlreadyTerminal", err)
	}
}

// TestSaveResults_EmptySequenceStillFencesOnMissing: a job that does not exist
// is ErrNotFound, empty sequence or not.
func TestSaveResults_EmptySequenceStillFencesOnMissing(t *testing.T) {
	store, ctx, _ := newSearchFenceStore(t, "fence_missing.db")

	err := store.SaveResults(ctx, "no-such-job", 1, iter.Seq[string](emptyIDs))
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("SaveResults(empty, missing job) = %v, want ErrNotFound", err)
	}
}

// TestSaveResults_ClaimLostAfterLastChunkIsReported: the fence also runs after
// the final chunk, so a claim reclaimed mid-stream is reported rather than
// silently accepted — matching the memory backend, which re-guards on every
// chunk boundary including the last.
func TestSaveResults_ClaimLostAfterLastChunkIsReported(t *testing.T) {
	store, ctx, jobID := newSearchFenceStore(t, "fence_midstream.db")

	// Yield exactly one full chunk so the chunk-boundary flush commits, then
	// bump the epoch out from under the caller — exactly as a concurrent
	// reclaim sweep would — and end the sequence. The trailing flush then has
	// an empty batch, which is the case that used to skip the fence entirely.
	ids := func(yield func(string) bool) {
		for i := 0; i < sqlite.SearchResultsChunkSizeForTest; i++ {
			if !yield(fmt.Sprintf("e%d", i)) {
				return
			}
		}
		if _, err := store.ClaimStale(ctx, -time.Hour, 10); err != nil {
			t.Errorf("ClaimStale during stream: %v", err)
		}
	}

	err := store.SaveResults(ctx, jobID, 1, iter.Seq[string](ids))
	if !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("SaveResults(claim lost after last chunk) = %v, want ErrStaleClaim", err)
	}
}
