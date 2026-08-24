package search_test

// Tests for the stale-job claim-then-FAIL reaper (task E3): FailStaleJobs
// claims stale RUNNING async-search jobs (spi.AsyncSearchStore.ClaimStale)
// and marks each FAILED via an epoch-fenced write, using a per-job tenant
// context reconstructed from SearchJob.TenantID. Each test below maps to one
// of the E3.1 scenarios in
// .superpowers/sdd/2026-08-22-472-search-spi-surface/task-E3-brief.md.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newReaperTestStore builds a memory AsyncSearchStore driven by a
// TestClock so staleness can be advanced deterministically instead of
// sleeping in real time.
func newReaperTestStore(t *testing.T) (spi.AsyncSearchStore, *memory.TestClock) {
	t.Helper()
	clk := memory.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	factory := memory.NewStoreFactory(memory.WithClock(clk))
	t.Cleanup(func() { factory.Close() })
	store, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	return store, clk
}

// createRunningJob persists a RUNNING job under tenant with the given
// CreateTime, returning its ID.
func createRunningJob(t *testing.T, store spi.AsyncSearchStore, tenant spi.TenantID, id string, createTime time.Time) {
	t.Helper()
	job := &spi.SearchJob{
		ID:         id,
		TenantID:   tenant,
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "widget", ModelVersion: "1"},
		Condition:  json.RawMessage(`{}`),
		SearchOpts: json.RawMessage(`{}`),
		CreateTime: createTime,
	}
	if err := store.CreateJob(tenantCtx(string(tenant)), job); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

// (a) a RUNNING job whose baseline (CreateTime, no heartbeat) is older than
// staleAfter is claimed and marked FAILED with a finish time and the
// sanitised fallback message.
func TestFailStaleJobs_ClaimsAndFailsStaleJob(t *testing.T) {
	store, clk := newReaperTestStore(t)
	const staleAfter = 5 * time.Minute

	createRunningJob(t, store, "tenant-a", "job-stale", clk.Now())
	clk.Advance(2 * staleAfter)

	n, err := search.FailStaleJobs(context.Background(), store, staleAfter, 10)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("FailStaleJobs claimed+failed %d jobs, want 1", n)
	}

	got, err := store.GetJob(tenantCtx("tenant-a"), "job-stale")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != "FAILED" {
		t.Errorf("job status = %q, want FAILED", got.Status)
	}
	if got.Error != search.JobFailureFallback() {
		t.Errorf("job error = %q, want sanitised fallback %q", got.Error, search.JobFailureFallback())
	}
	if got.FinishTime == nil {
		t.Error("job FinishTime is nil, want a stamped finish time")
	}
}

// (b) a RUNNING job whose owner is alive and heartbeating recently must
// never be claimed — the reaper must not fight a live executor.
func TestFailStaleJobs_FreshHeartbeatJobUntouched(t *testing.T) {
	store, clk := newReaperTestStore(t)
	const staleAfter = 5 * time.Minute

	createRunningJob(t, store, "tenant-a", "job-fresh", clk.Now())
	// Advance well past staleAfter from CreateTime, then heartbeat right
	// before the reaper runs — the fresh heartbeat resets the baseline so
	// the job must survive despite its old CreateTime.
	clk.Advance(2 * staleAfter)
	if err := store.Heartbeat(tenantCtx("tenant-a"), "job-fresh", 1); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	clk.Advance(1 * time.Minute) // still well under staleAfter since the heartbeat

	n, err := search.FailStaleJobs(context.Background(), store, staleAfter, 10)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("FailStaleJobs claimed+failed %d jobs, want 0 (fresh heartbeat)", n)
	}

	got, err := store.GetJob(tenantCtx("tenant-a"), "job-fresh")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != "RUNNING" {
		t.Errorf("job status = %q, want RUNNING (untouched)", got.Status)
	}
}

// (c) a job already in a terminal status (SUCCESSFUL) is never claimed or
// rewritten, regardless of staleness.
func TestFailStaleJobs_SuccessfulJobUntouched(t *testing.T) {
	store, clk := newReaperTestStore(t)
	const staleAfter = 5 * time.Minute

	createRunningJob(t, store, "tenant-a", "job-done", clk.Now())
	finishTime := clk.Now()
	if err := store.UpdateJobStatus(tenantCtx("tenant-a"), "job-done", 1, "SUCCESSFUL", 3, "", finishTime, 42); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	clk.Advance(2 * staleAfter)

	n, err := search.FailStaleJobs(context.Background(), store, staleAfter, 10)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("FailStaleJobs claimed+failed %d jobs, want 0 (terminal)", n)
	}

	got, err := store.GetJob(tenantCtx("tenant-a"), "job-done")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != "SUCCESSFUL" || got.Error != "" || got.ResultCount != 3 {
		t.Errorf("terminal job was rewritten: status=%q error=%q resultCount=%d", got.Status, got.Error, got.ResultCount)
	}
}

// (d) a job claimed by FailStaleJobs but reclaimed by a concurrent claimer
// before FailStaleJobs' own follow-up write lands loses the race
// (ErrStaleClaim) — tolerated, counted as skipped, not surfaced as an
// error and not left FAILED.
func TestFailStaleJobs_LostFenceRaceCountsAsSkipped(t *testing.T) {
	store, clk := newReaperTestStore(t)
	const staleAfter = 5 * time.Minute

	createRunningJob(t, store, "tenant-a", "job-raced", clk.Now())
	clk.Advance(2 * staleAfter)

	racing := &raceInjectingStore{AsyncSearchStore: store, targetJobID: "job-raced"}

	n, err := search.FailStaleJobs(context.Background(), racing, staleAfter, 10)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("FailStaleJobs reported %d successful failures, want 0 (lost the fence race)", n)
	}
	if !racing.triggered {
		t.Fatal("test precondition: the race injection never fired")
	}

	// The job was NOT marked FAILED by the losing write — the concurrent
	// claimer's reclaim left it RUNNING under a newer epoch.
	got, err := store.GetJob(tenantCtx("tenant-a"), "job-raced")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != "RUNNING" {
		t.Errorf("job status = %q, want RUNNING (the reclaimer, not FailStaleJobs, owns it now)", got.Status)
	}
}

// raceInjectingStore wraps a real spi.AsyncSearchStore and, on the first
// UpdateJobStatus call for targetJobID, reclaims that job via a second
// ClaimStale (bumping its epoch) before forwarding the original call —
// simulating a concurrent reaper/claimer winning the race between
// FailStaleJobs' own claim and its follow-up write.
type raceInjectingStore struct {
	spi.AsyncSearchStore
	targetJobID string
	triggered   bool
}

func (r *raceInjectingStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if jobID == r.targetJobID && !r.triggered {
		r.triggered = true
		// A negative staleAfter pushes the cutoff into the future relative
		// to any baseline stamped "now" by the virtual TestClock (which does
		// not advance between this claim and the one FailStaleJobs already
		// made), so this reclaims the job unconditionally — simulating a
		// second claimer racing in right now.
		if _, err := r.AsyncSearchStore.ClaimStale(context.Background(), -time.Hour, 10); err != nil {
			return err
		}
	}
	return r.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

// (e) tenant reconstruction: ClaimStale is cross-tenant, but the follow-up
// FAILED write for each claimed job must land under that job's own tenant
// — never leak into another tenant's context or a tenant-less one.
func TestFailStaleJobs_TenantReconstructionPerJob(t *testing.T) {
	store, clk := newReaperTestStore(t)
	const staleAfter = 5 * time.Minute

	createRunningJob(t, store, "tenant-a", "job-a", clk.Now())
	createRunningJob(t, store, "tenant-b", "job-b", clk.Now())
	clk.Advance(2 * staleAfter)

	n, err := search.FailStaleJobs(context.Background(), store, staleAfter, 10)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 2 {
		t.Fatalf("FailStaleJobs claimed+failed %d jobs, want 2", n)
	}

	gotA, err := store.GetJob(tenantCtx("tenant-a"), "job-a")
	if err != nil {
		t.Fatalf("GetJob(job-a) under tenant-a: %v", err)
	}
	if gotA.Status != "FAILED" {
		t.Errorf("job-a status = %q, want FAILED", gotA.Status)
	}

	gotB, err := store.GetJob(tenantCtx("tenant-b"), "job-b")
	if err != nil {
		t.Fatalf("GetJob(job-b) under tenant-b: %v", err)
	}
	if gotB.Status != "FAILED" {
		t.Errorf("job-b status = %q, want FAILED", gotB.Status)
	}

	// job-a must not be visible/mutable under tenant-b's context and
	// vice versa — the memory store is tenant-scoped per resolveTenant,
	// so a cross-tenant GetJob is a miss (ErrNotFound), not a leak.
	if _, err := store.GetJob(tenantCtx("tenant-b"), "job-a"); err == nil {
		t.Error("job-a was visible under tenant-b's context — cross-tenant leak")
	}
	if _, err := store.GetJob(tenantCtx("tenant-a"), "job-b"); err == nil {
		t.Error("job-b was visible under tenant-a's context — cross-tenant leak")
	}
}

// (f) the claim is bounded: FailStaleJobs never pulls more than batch jobs
// in one call.
func TestFailStaleJobs_BoundedBatch(t *testing.T) {
	store, clk := newReaperTestStore(t)
	const staleAfter = 5 * time.Minute

	for i := 0; i < 5; i++ {
		createRunningJob(t, store, "tenant-a", "job-"+string(rune('a'+i)), clk.Now())
	}
	clk.Advance(2 * staleAfter)

	n, err := search.FailStaleJobs(context.Background(), store, staleAfter, 2)
	if err != nil {
		t.Fatalf("FailStaleJobs: %v", err)
	}
	if n != 2 {
		t.Fatalf("FailStaleJobs claimed+failed %d jobs, want batch-bounded 2", n)
	}
}
