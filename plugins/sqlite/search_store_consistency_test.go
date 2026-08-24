package sqlite_test

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestGetResultIDs_TotalDescribesTheReturnedPage: reading a non-terminal job
// is contract-supported ("Reading a non-terminal job answers with the results
// saved so far"), so a SaveResults chunk can commit while GetResultIDs is
// running. Reading the count and the page as two unsynchronised statements
// lets a chunk land between them, so `total` describes a different snapshot
// than the returned page — the memory backend takes both under one RLock.
//
// The writer streams enough chunks that the window is hit reliably without the
// fix; with the fix the assertion holds on every iteration, so the test cannot
// flake red.
func TestGetResultIDs_TotalDescribesTheReturnedPage(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "result_consistency.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-consistency")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	jobID := "job-consistency"
	if err := store.CreateJob(ctx, &spi.SearchJob{
		ID:         jobID,
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
		CreateTime: time.Now(),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	const chunks = 12
	total := chunks * sqlite.SearchResultsChunkSizeForTest

	saved := make(chan error, 1)
	go func() {
		ids := func(yield func(string) bool) {
			for i := 0; i < total; i++ {
				if !yield(fmt.Sprintf("e%05d", i)) {
					return
				}
			}
		}
		saved <- store.SaveResults(ctx, jobID, 1, iter.Seq[string](ids))
	}()

	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-saved:
			if err != nil {
				t.Fatalf("SaveResults: %v", err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("SaveResults did not finish within 30s")
		}
		ids, reported, err := store.GetResultIDs(ctx, jobID, 0, total+1)
		if err != nil {
			t.Fatalf("GetResultIDs: %v", err)
		}
		if reported != len(ids) {
			t.Fatalf("GetResultIDs(offset=0, limit>total): total=%d but page has %d ids — "+
				"the count and the page were read from different snapshots", reported, len(ids))
		}
	}
}

// TestClaimStale_TakesOldestJobsFirst: a limit-capped sweep must take the
// oldest jobs, not an arbitrary subset. Postgres orders the candidate scan by
// created_at; sqlite had no ORDER BY, so which jobs a partial sweep took was
// whatever order the storage engine happened to produce. The jobs are inserted
// newest-first so insertion order (which an unordered scan follows) disagrees
// with creation order.
func TestClaimStale_TakesOldestJobsFirst(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "claim_order.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-claim")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	now := time.Now()
	// Insertion order is newest → oldest.
	jobs := []struct {
		id      string
		created time.Time
	}{
		{"job-newest", now},
		{"job-middle", now.Add(-2 * time.Hour)},
		{"job-oldest", now.Add(-3 * time.Hour)},
	}
	for _, j := range jobs {
		if err := store.CreateJob(ctx, &spi.SearchJob{
			ID:         j.id,
			Status:     "RUNNING",
			ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
			CreateTime: j.created,
		}); err != nil {
			t.Fatalf("CreateJob(%s): %v", j.id, err)
		}
	}

	// Negative staleAfter puts the cutoff in the future, so all three qualify.
	claimed, err := store.ClaimStale(ctx, -time.Hour, 2)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	got := make([]string, 0, len(claimed))
	for _, j := range claimed {
		got = append(got, j.ID)
	}
	sort.Strings(got)
	want := []string{"job-middle", "job-oldest"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ClaimStale(limit=2) claimed %v, want the two oldest %v", got, want)
	}
}
