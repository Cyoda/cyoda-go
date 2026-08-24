package sqlite_test

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

// TestGetResultIDs_PaginationCheckedBeforeTenantResolution pins the order the
// three backends share: offset/limit are pure argument validation, decidable
// without touching any state, so they are rejected before the tenant is
// resolved. memory checked the tenant first, so a request with both inputs bad
// reported a different error there than here.
func TestGetResultIDs_PaginationCheckedBeforeTenantResolution(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "validation_order.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	store, err := factory.AsyncSearchStore(testCtx("tenant-order"))
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	// No user context at all AND invalid pagination.
	_, _, err = store.GetResultIDs(context.Background(), "job-x", -1, 0)
	if err == nil {
		t.Fatal("GetResultIDs with no user context and invalid pagination returned no error")
	}
	if !strings.Contains(err.Error(), "offset") && !strings.Contains(err.Error(), "limit") {
		t.Errorf("GetResultIDs reported %q, want the pagination error", err)
	}
}

// TestGetResultIDs_DoesNotHoldTheWriterConnection: GetResultIDs is a pure
// read, and reads a caller-controlled number of rows (`limit` comes straight
// off `GET .../results?limit=N`). Its three statements run in ONE transaction
// so the count and the page describe a single snapshot — which means the
// connection is checked out for the whole scan. On the writer *sql.DB, capped
// at SetMaxOpenConns(1), that is the sole connection every SaveResults commit,
// Heartbeat and entity write also needs: one client polling a large job with a
// big limit would stall every write in the process behind it. This is the exact
// starvation GetPage and the version reads were moved onto readDB to avoid.
//
// The writer's only connection is deliberately occupied here, so the assertion
// is structural rather than timing-based: if GetResultIDs still ran on the
// writer pool it could not proceed at all.
func TestGetResultIDs_DoesNotHoldTheWriterConnection(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "result_reader.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-reader")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	jobID := "job-reader"
	if err := store.CreateJob(ctx, &spi.SearchJob{
		ID:         jobID,
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
		CreateTime: time.Now(),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	want := []string{"e0", "e1", "e2"}
	if err := store.SaveResults(ctx, jobID, 1, slices.Values(want)); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}

	// Occupy the writer pool's single connection, standing in for any
	// in-flight write (a SaveResults chunk, an entity Save).
	writerTx, err := sqlite.DBForTest(factory).BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("occupy writer connection: %v", err)
	}
	defer writerTx.Rollback()

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ids, total, err := store.GetResultIDs(readCtx, jobID, 0, 10)
	if err != nil {
		t.Fatalf("GetResultIDs blocked on the writer pool's sole connection (%v) — "+
			"a pure read must run on readDB, not queue behind every write", err)
	}
	if total != len(want) || len(ids) != len(want) {
		t.Fatalf("GetResultIDs = %v (total %d), want %v (total %d)", ids, total, want, len(want))
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

// TestClaimStale_RejectsNonPositiveLimit: "up to limit jobs" has no meaning
// below 1, and the three backends disagreed about what it meant — memory
// silently claimed nothing, postgres raised a raw driver error, and sqlite
// passed the value straight into `LIMIT ?`, where SQLite defines LIMIT -1 as
// UNBOUNDED. An unbounded claim epoch-bumps every RUNNING job in the cluster,
// dispossessing every live executor at once. All three now reject it, matching
// GetResultIDs' documented "limit >= 1; a violation returns an error".
func TestClaimStale_RejectsNonPositiveLimit(t *testing.T) {
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, "claim_limit.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-claim-limit")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	if err := store.CreateJob(ctx, &spi.SearchJob{
		ID:         "job-live",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
		CreateTime: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	for _, limit := range []int{0, -1} {
		claimed, err := store.ClaimStale(ctx, -time.Hour, limit)
		if err == nil {
			t.Errorf("ClaimStale(limit=%d) returned %d jobs and no error, want an error", limit, len(claimed))
		}
		if len(claimed) != 0 {
			t.Errorf("ClaimStale(limit=%d) claimed %d jobs, want none", limit, len(claimed))
		}
	}

	job, err := store.GetJob(ctx, "job-live")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Epoch != 1 {
		t.Errorf("a rejected ClaimStale bumped the epoch to %d, want 1", job.Epoch)
	}
	if job.HeartbeatTime != nil {
		t.Error("a rejected ClaimStale stamped HeartbeatTime")
	}
}
