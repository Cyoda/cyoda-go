package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// StaleClaimBatch caps how many stale jobs a single ReclaimStaleJobs call
// claims. Fixed rather than configurable: it bounds one reaper tick's work
// however large staleAfter's backlog has grown, so a burst of dead jobs cannot
// turn one tick into an unbounded claim-and-execute loop.
const StaleClaimBatch = 100

// ReclaimStaleJobs claims stale or released RUNNING async-search jobs and
// re-executes them on this node, or fails those past the attempt cap. A
// crashed node's job is completed by a live node rather than left failed.
// Returns (reenqueued, failed).
//
// Self-executing stores own their own recovery — skipped, exactly as
// SubmitAsync skips its own execution goroutine for them.
//
// The claim is bounded by this node's free capacity (pool.Cap() minus the
// current registry size), so a saturated node claims nothing rather than
// taking work it cannot start. maxAttempts bounds StaleClaims (executor
// losses), not Epoch: a graceful handoff (Release then claim) never advances
// a job toward being failed.
func (s *SearchService) ReclaimStaleJobs(ctx context.Context, staleAfter time.Duration, maxAttempts int) (int, int, error) {
	// Self-executing stores own their own recovery — mirrors SubmitAsync's
	// identical type-assertion guard (service.go), which skips the in-process
	// execution goroutine for the same reason: calling SaveResults,
	// UpdateJobStatus or ClearResults on one of these stores is an error, and
	// their jobs never receive an engine heartbeat, so ClaimStale's
	// COALESCE(heartbeat_time, created_at) baseline would make every healthy
	// job of theirs look stale by construction. Fail closed: skip entirely.
	if _, ok := s.searchStore.(spi.SelfExecutingSearchStore); ok {
		return 0, 0, nil
	}

	headroom := s.asyncPool().Cap() - s.registrySize()
	if headroom <= 0 {
		return 0, 0, nil
	}
	limit := StaleClaimBatch
	if headroom < limit {
		limit = headroom
	}

	jobs, err := s.searchStore.ClaimStale(ctx, staleAfter, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to claim stale search jobs: %w", err)
	}

	reenqueued, failed := 0, 0
	for _, job := range jobs {
		tenantCtx := common.SystemUserContext(job.TenantID)

		// Attempt cap: bound on executor losses, not on graceful handoffs.
		if int64(maxAttempts) <= job.StaleClaims {
			if werr := s.searchStore.UpdateJobStatus(tenantCtx, job.ID, job.Epoch, "FAILED", 0, jobAttemptsExhausted, time.Now(), 0); werr != nil {
				if errors.Is(werr, spi.ErrAlreadyTerminal) || errors.Is(werr, spi.ErrStaleClaim) {
					slog.Warn("attempt-cap fail lost the race; job already settled", "pkg", "search", "jobID", job.ID, "err", werr)
					continue
				}
				slog.Error("failed to fail crash-looping search job", "pkg", "search", "jobID", job.ID, "err", werr)
				continue
			}
			slog.Warn("async search job abandoned after repeated executor loss", "pkg", "search", "jobID", job.ID, "epoch", job.Epoch, "staleClaims", job.StaleClaims)
			failed++
			continue
		}

		// Clear the prior epoch's partial rows before re-running. On failure,
		// release (uncounted) so a peer or the next sweep retries — never
		// enqueue over unknown residue (fail closed).
		if cerr := s.searchStore.ClearResults(tenantCtx, job.ID); cerr != nil {
			slog.Error("failed to clear results before reclaim; releasing", "pkg", "search", "jobID", job.ID, "err", cerr)
			if rerr := s.searchStore.Release(tenantCtx, job.ID, job.Epoch); rerr != nil {
				slog.Error("failed to release after ClearResults error", "pkg", "search", "jobID", job.ID, "err", rerr)
			}
			continue
		}

		if s.reenqueueClaimed(job) {
			reenqueued++
		}
	}
	return reenqueued, failed, nil
}

// reenqueueClaimed re-runs a claimed job on this node at its claimed epoch.
// Returns true if the job entered the pool. On any failure it either fails the
// job (a genuine defect — an undecodable stored job) or releases it (transient
// — queue full), never leaves it silently RUNNING with no executor.
func (s *SearchService) reenqueueClaimed(job *spi.SearchJob) bool {
	uc := common.SystemUserContextValue(job.TenantID) // *spi.UserContext for the job's tenant
	baseCtx := spi.WithUserContext(context.Background(), uc)
	if scoper, ok := s.searchStore.(asyncScanScoper); ok {
		baseCtx = scoper.AsyncScanContext(baseCtx)
	}
	tenantCtx := spi.WithUserContext(context.Background(), uc)

	cond, opts, orderBy, decErr := decodeStoredJob(job)
	if decErr != nil {
		// A stored job that cannot be decoded is a defect, not a runtime
		// condition: fail it at the claimed epoch rather than loop on it.
		slog.Error("failed to decode claimed search job; failing", "pkg", "search", "jobID", job.ID, "err", decErr)
		if werr := s.searchStore.UpdateJobStatus(tenantCtx, job.ID, job.Epoch, "FAILED", 0, jobFailureFallback, time.Now(), 0); werr != nil {
			slog.Error("failed to fail undecodable claimed job", "pkg", "search", "jobID", job.ID, "err", werr)
		}
		return false
	}

	jobCtx, cancel := context.WithCancelCause(baseCtx)
	handle := s.registerReclaim(job.ID, cancel, uc, job.Epoch)
	s.startHeartbeat(jobCtx, cancel, job.ID, job.Epoch)

	submitErr := s.asyncPool().Submit(func() {
		s.runAsyncJob(jobCtx, cancel, handle, job.ID, job.Epoch, job.ModelRef, cond, opts, orderBy)
	})
	if submitErr != nil {
		cancel(nil)
		s.deregisterJobHandle(job.ID, handle)
		// Release (uncounted) so a peer with capacity, or this node's next
		// sweep, takes it without waiting for staleness.
		if rerr := s.searchStore.Release(tenantCtx, job.ID, job.Epoch); rerr != nil {
			slog.Warn("failed to release reclaimed job after queue-full", "pkg", "search", "jobID", job.ID, "err", rerr)
		}
		return false
	}
	return true
}

// decodeStoredJob reconstructs the condition and search options SubmitAsync
// persisted on the job row. job.Condition is the client predicate in DOMAIN
// wire syntax (predicate.ParseCondition), and job.SearchOpts is the same
// {limit, pointInTime, orderBy} envelope SubmitAsync marshals — orderBy stored
// as already-resolved spi.OrderSpec values (PascalCase field names, no json
// tags), so it is returned as the resolvedOrderBy the executor scans under
// rather than being re-resolved against the schema.
func decodeStoredJob(job *spi.SearchJob) (predicate.Condition, SearchOptions, []spi.OrderSpec, error) {
	cond, err := predicate.ParseCondition(job.Condition)
	if err != nil {
		return nil, SearchOptions{}, nil, fmt.Errorf("failed to parse stored search condition: %w", err)
	}
	var stored struct {
		Limit       int             `json:"limit"`
		PointInTime *time.Time      `json:"pointInTime,omitempty"`
		OrderBy     []spi.OrderSpec `json:"orderBy,omitempty"`
	}
	if err := json.Unmarshal(job.SearchOpts, &stored); err != nil {
		return nil, SearchOptions{}, nil, fmt.Errorf("failed to unmarshal stored search options: %w", err)
	}
	opts := SearchOptions{Limit: stored.Limit, PointInTime: stored.PointInTime}
	return cond, opts, stored.OrderBy, nil
}
