package memory_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestClaimStale_TakesOldestJobsFirst mirrors the sqlite test of the same
// name: a limit-capped sweep must take the OLDEST stale jobs, not an
// arbitrary subset. sqlite and postgres order the candidate scan by creation
// time; memory iterated its Go map, whose order is deliberately randomised —
// so with more stale jobs than the reaper's batch (search.StaleClaimBatch =
// 100) the oldest crashed job could be skipped on every sweep, indefinitely.
//
// Enough jobs are seeded that a random subset almost never happens to be the
// oldest three: C(12,3) = 220 orderings, so an unordered implementation fails
// this on well over 99% of runs.
func TestClaimStale_TakesOldestJobsFirst(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now()
	const total = 12
	// Insertion order is newest → oldest, so insertion order disagrees with
	// creation order.
	for i := 0; i < total; i++ {
		if err := store.CreateJob(ctx, &spi.SearchJob{
			ID:         fmt.Sprintf("job-%02d", i),
			Status:     "RUNNING",
			ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
			CreateTime: now.Add(-time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("CreateJob(%d): %v", i, err)
		}
	}

	// Negative staleAfter puts the cutoff in the future, so all of them qualify.
	claimed, err := store.ClaimStale(ctx, -time.Hour, 3)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	got := make([]string, 0, len(claimed))
	for _, j := range claimed {
		got = append(got, j.ID)
	}
	sort.Strings(got)
	// The three oldest are the three highest indices (created furthest back).
	want := []string{"job-09", "job-10", "job-11"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ClaimStale(limit=3) claimed %v, want the three oldest %v", got, want)
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
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

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

	// The rejected calls must not have touched the job.
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

// TestClaimStale_OldestFirstTieBreaksOnID: equal CreateTime must not
// reintroduce map-order nondeterminism — the tiebreak is the job ID, so the
// claimed set is the same on every run.
func TestClaimStale_OldestFirstTieBreaksOnID(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	created := time.Now().Add(-time.Hour)
	for i := 0; i < 8; i++ {
		if err := store.CreateJob(ctx, &spi.SearchJob{
			ID:         fmt.Sprintf("job-%02d", i),
			Status:     "RUNNING",
			ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
			CreateTime: created,
		}); err != nil {
			t.Fatalf("CreateJob(%d): %v", i, err)
		}
	}

	claimed, err := store.ClaimStale(ctx, -time.Hour, 3)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	got := make([]string, 0, len(claimed))
	for _, j := range claimed {
		got = append(got, j.ID)
	}
	sort.Strings(got)
	want := []string{"job-00", "job-01", "job-02"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ClaimStale(limit=3) with equal CreateTime claimed %v, want the ID-ordered %v", got, want)
	}
}
