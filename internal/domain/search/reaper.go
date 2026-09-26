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

	// Two passes. The first starts every job this node will run — register,
	// heartbeat, enqueue — and issues no main-pool statement. The second
	// writes the outcome of each job it will not run. Those writes share the
	// store's main pool with entity transactions, so a blocked one must not
	// delay the heartbeat of a runnable job later in the batch.
	reenqueued, failed := 0, 0
	var settle []func() bool // reports whether the job was failed
	for _, job := range jobs {
		// Attempt cap: bound on executor losses, not on graceful handoffs.
		if int64(maxAttempts) <= job.StaleClaims {
			settle = append(settle, func() bool { return s.failAttemptsExhausted(job) })
			continue
		}
		started, unrun := s.reenqueueClaimed(job)
		if started {
			reenqueued++
			continue
		}
		settle = append(settle, func() bool { unrun(); return false })
	}
	for _, write := range settle {
		if write() {
			failed++
		}
	}
	return reenqueued, failed, nil
}

// failAttemptsExhausted fails a claimed job that has reached the attempt cap,
// at its claimed epoch, and reports whether the write landed.
func (s *SearchService) failAttemptsExhausted(job *spi.SearchJob) bool {
	tenantCtx := common.SystemUserContext(job.TenantID)
	if werr := s.searchStore.UpdateJobStatus(tenantCtx, job.ID, job.Epoch, "FAILED", 0, jobAttemptsExhausted, time.Now(), 0); werr != nil {
		if errors.Is(werr, spi.ErrAlreadyTerminal) || errors.Is(werr, spi.ErrStaleClaim) {
			slog.Warn("attempt-cap fail lost the race; job already settled", "pkg", "search", "jobID", job.ID, "err", werr)
			return false
		}
		slog.Error("failed to fail crash-looping search job", "pkg", "search", "jobID", job.ID, "err", werr)
		return false
	}
	slog.Warn("async search job abandoned after repeated executor loss", "pkg", "search", "jobID", job.ID, "epoch", job.Epoch, "staleClaims", job.StaleClaims)
	return true
}

// reenqueueClaimed re-runs a claimed job on this node at its claimed epoch,
// and issues no main-pool statement itself. It reports whether the job entered
// the pool. The heartbeat starts here; the prior epoch's results are cleared
// on the worker (clearReclaimedResults).
//
// A job that did not enter the pool comes back with unrun, the write that
// settles it, for the caller to send once every runnable job of the batch has
// started: it fails the job (a genuine defect — an undecodable stored job) or
// releases it (transient — queue full). A job is never left silently RUNNING
// with no executor.
func (s *SearchService) reenqueueClaimed(job *spi.SearchJob) (started bool, unrun func()) {
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
		return false, func() {
			if werr := s.searchStore.UpdateJobStatus(tenantCtx, job.ID, job.Epoch, "FAILED", 0, jobFailureFallback, time.Now(), 0); werr != nil {
				slog.Error("failed to fail undecodable claimed job", "pkg", "search", "jobID", job.ID, "err", werr)
			}
		}
	}

	jobCtx, cancel := context.WithCancelCause(baseCtx)
	handle := s.registerReclaim(job.ID, cancel, uc, job.Epoch)
	s.startHeartbeat(jobCtx, cancel, job.ID, job.Epoch)

	submitErr := s.asyncPool().Submit(func() {
		if !s.clearReclaimedResults(jobCtx, cancel, handle, job) {
			return
		}
		s.runAsyncJob(jobCtx, cancel, handle, job.ID, job.Epoch, job.ModelRef, cond, opts, orderBy)
	})
	if submitErr != nil {
		cancel(nil)
		s.deregisterJobHandle(job.ID, handle)
		// Release (uncounted) so a peer with capacity, or this node's next
		// sweep, takes it without waiting for staleness.
		return false, func() {
			if rerr := s.searchStore.Release(tenantCtx, job.ID, job.Epoch); rerr != nil {
				slog.Warn("failed to release reclaimed job after queue-full", "pkg", "search", "jobID", job.ID, "err", rerr)
			}
		}
	}
	return true, nil
}

// clearReclaimedResults deletes the prior epoch's partial results before a
// reclaimed job re-runs, and reports whether the job may run. It runs on the
// worker, under the heartbeat reenqueueClaimed has already started: the clear
// shares the store's main pool with entity transactions, and neither the
// reclaim sweep nor the job's liveness may wait on that pool.
//
// The clear is fenced by the claimed epoch: a clear that lands after the job
// was taken from this node, or settled, is refused and deletes nothing, so a
// late clear cannot wipe the rows the current owner saved.
//
// A job that does not run is deregistered, and is never run over unknown
// residue (fail closed). Whether it is also released depends on why:
//   - The clear failed: released (uncounted, fenced like the clear), so a
//     peer or the next sweep retries it.
//   - The clear was refused (taken by a newer epoch, terminal, or gone):
//     nothing is this node's to hand back, so no release.
//   - The job's context ended first: the cause owns the next step. A shutdown
//     release (errJobReleased) has already released it, and a self-reclaim
//     (errJobSuperseded) holds it at a newer epoch, so neither is released
//     again. Any other end — a refused heartbeat, an in-process cancel — gets
//     the fenced release, which the store refuses if the job is no longer
//     this epoch's.
func (s *SearchService) clearReclaimedResults(jobCtx context.Context, cancel context.CancelCauseFunc, handle *asyncJobHandle, job *spi.SearchJob) bool {
	if jobCtx.Err() == nil {
		cerr := s.searchStore.ClearResults(jobCtx, job.ID, job.Epoch)
		switch {
		case cerr == nil:
			return true
		case errors.Is(cerr, spi.ErrAlreadyTerminal):
			slog.Debug("reclaimed job was settled before it ran", "pkg", "search", "jobID", job.ID)
			s.dropReclaim(cancel, handle, job)
			return false
		case errors.Is(cerr, spi.ErrStaleClaim), errors.Is(cerr, spi.ErrNotFound):
			slog.Warn("reclaimed job was taken or removed before it ran", "pkg", "search", "jobID", job.ID, "err", cerr)
			s.dropReclaim(cancel, handle, job)
			return false
		case jobCtx.Err() == nil:
			slog.Error("failed to clear results before reclaim; releasing", "pkg", "search", "jobID", job.ID, "err", cerr)
			s.dropReclaim(cancel, handle, job)
			s.releaseUnrun(job)
			return false
		}
	}
	cause := context.Cause(jobCtx)
	slog.Debug("reclaimed job ended before it ran", "pkg", "search", "jobID", job.ID, "cause", cause)
	s.dropReclaim(cancel, handle, job)
	if !errors.Is(cause, errJobReleased) && !errors.Is(cause, errJobSuperseded) {
		s.releaseUnrun(job)
	}
	return false
}

// dropReclaim ends a reclaimed job that will not run on this node: it stops
// the heartbeat and removes this handle's registration.
func (s *SearchService) dropReclaim(cancel context.CancelCauseFunc, handle *asyncJobHandle, job *spi.SearchJob) {
	cancel(nil)
	s.deregisterJobHandle(job.ID, handle)
}

// releaseUnrun hands a reclaimed job that did not run back for the next claim,
// uncounted and fenced by its claimed epoch. A refusal means the job is no
// longer this epoch's to hand back — an ordinary outcome, not a fault.
func (s *SearchService) releaseUnrun(job *spi.SearchJob) {
	tenantCtx := common.SystemUserContext(job.TenantID)
	rerr := s.searchStore.Release(tenantCtx, job.ID, job.Epoch)
	switch {
	case rerr == nil:
	case errors.Is(rerr, spi.ErrStaleClaim), errors.Is(rerr, spi.ErrAlreadyTerminal), errors.Is(rerr, spi.ErrNotFound):
		slog.Debug("reclaimed job that did not run is no longer this epoch's", "pkg", "search", "jobID", job.ID, "err", rerr)
	default:
		slog.Warn("failed to release reclaimed job that did not run", "pkg", "search", "jobID", job.ID, "err", rerr)
	}
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
