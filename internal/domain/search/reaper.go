package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
)

// StaleClaimBatch caps how many stale jobs a single FailStaleJobs call
// claims. Fixed rather than configurable — like scheduler.BatchSize's
// role for ScanDue, this bounds one reaper tick's work regardless of how
// large staleAfter's backlog has grown, so a burst of dead jobs cannot turn
// one tick into an unbounded claim-and-write loop. Matches
// CYODA_SCHEDULER_BATCH_SIZE's documented default (100).
const StaleClaimBatch = 100

// FailStaleJobs claims up to batch stale RUNNING async-search jobs (see
// spi.AsyncSearchStore.ClaimStale's staleness definition: HeartbeatTime, or
// CreateTime as the baseline when never heartbeated, older than staleAfter)
// and marks each FAILED via an epoch-fenced UpdateJobStatus, using the
// epoch ClaimStale returned for that job. A job whose owner is still
// heartbeating is never claimed — ClaimStale itself enforces that, so this
// reaper cannot fight a live executor for a job it still owns.
//
// Interim disposition for this milestone: a stale job — its owning executor
// stopped heartbeating, most likely because the node holding it crashed or
// was killed — is failed outright with the sanitised fallback message, not
// re-executed. A follow-up (already filed) replaces this with re-execution
// elsewhere in the cluster; this call does not attempt it.
//
// ClaimStale is cross-tenant (called here with ctx, the same
// background/tenant-less context ReapExpired and ScheduledTaskStore.ScanDue
// use); the jobs it returns carry TenantID, but every store write after
// that goes through the tenant-scoped store. So each claimed job's
// follow-up write runs under a context reconstructed from that job's own
// TenantID via scheduler.SystemUserContext — the same system-principal
// construction the scheduler's background fire path already uses, not a
// second bespoke one — which also keeps one tenant's claimed job from ever
// being written under another tenant's (or no tenant's) context.
//
// A write that loses the race against the job's own terminal write
// (spi.ErrAlreadyTerminal) or a concurrent reclaim (spi.ErrStaleClaim) means
// another writer legitimately won — logged at Warn and counted as skipped,
// not an error and not latched against health. Returns the number of jobs
// this call successfully marked FAILED (skipped races are not counted); a
// non-nil error means ClaimStale itself failed, not that a subset of writes
// failed.
func FailStaleJobs(ctx context.Context, store spi.AsyncSearchStore, staleAfter time.Duration, batch int) (int, error) {
	jobs, err := store.ClaimStale(ctx, staleAfter, batch)
	if err != nil {
		return 0, fmt.Errorf("failed to claim stale search jobs: %w", err)
	}

	now := time.Now()
	failed := 0
	for _, job := range jobs {
		tenantCtx := scheduler.SystemUserContext(job.TenantID)
		writeErr := store.UpdateJobStatus(tenantCtx, job.ID, job.Epoch, "FAILED", 0, jobFailureFallback, now, 0)
		if writeErr == nil {
			failed++
			continue
		}
		if errors.Is(writeErr, spi.ErrAlreadyTerminal) || errors.Is(writeErr, spi.ErrStaleClaim) {
			slog.Warn("stale search job terminal write lost the race; another writer already settled it",
				"pkg", "search", "jobID", job.ID, "err", writeErr)
			continue
		}
		slog.Error("failed to fail stale search job", "pkg", "search", "jobID", job.ID, "err", writeErr)
	}
	return failed, nil
}
