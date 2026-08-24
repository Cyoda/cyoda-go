package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
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
// TenantID via common.SystemUserContext — the same system-principal
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
	// Self-executing stores own their own recovery — mirrors SubmitAsync's
	// identical type-assertion guard (service.go), which skips the
	// in-process execution goroutine for the same reason: calling
	// SaveResults or UpdateJobStatus on one of these stores is an error.
	// SelfExecutingSearchStore's doc comment only says such a store MAY
	// no-op ClaimStale/UpdateJobStatus/ClearResults — an unenforced
	// negative contract. Without this guard, a self-executing store that
	// never stamps HeartbeatTime (because the engine's own heartbeat loop
	// is skipped for it, same guard, same reason) would make ClaimStale's
	// COALESCE(heartbeat_time, created_at) baseline mark every one of its
	// healthy jobs stale after staleAfter by construction, and this reaper
	// would FAIL them out from under the store's own pipeline — a
	// cross-tenant false failure the engine has the type information to
	// prevent outright. Fail closed: skip reaping entirely rather than
	// depend on the permissive contract being honoured.
	if _, ok := store.(spi.SelfExecutingSearchStore); ok {
		return 0, nil
	}

	jobs, err := store.ClaimStale(ctx, staleAfter, batch)
	if err != nil {
		return 0, fmt.Errorf("failed to claim stale search jobs: %w", err)
	}

	now := time.Now()
	failed := 0
	for _, job := range jobs {
		tenantCtx := common.SystemUserContext(job.TenantID)
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
