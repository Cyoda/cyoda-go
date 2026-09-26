package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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
// Returns the number of jobs re-enqueued.
//
// Self-executing stores own their own recovery — skipped, exactly as
// SubmitAsync skips its own execution goroutine for them.
//
// The claim is bounded by this node's free capacity (pool.Cap() minus the
// current registry size), so a saturated node claims nothing rather than
// taking work it cannot start. maxAttempts bounds StaleClaims (executor
// losses), not Epoch: a graceful handoff (Release then claim) never advances
// a job toward being failed.
//
// The sweep never waits on the store's main pool, which it shares with entity
// transactions. It claims on the store's own path (ClaimStale), then starts
// every job it will run — register, heartbeat, enqueue — with no main-pool
// statement. The writes it owes the jobs it will not run (the attempt-cap
// FAILED write, the FAILED write for an undecodable job, the queue-full
// Release) go to the second pass: one goroutine per node that sends them
// under ctx. A sweep that finds the second pass in flight hands its writes to
// it and returns; none is dropped while ctx lives. Every owed write is fenced
// by the claimed epoch, so one that is never sent loses nothing: its job
// stays RUNNING, goes stale, and is claimed again. Once ctx ends, the second
// pass sends nothing more and returns; WaitReclaimSecondPass awaits it.
func (s *SearchService) ReclaimStaleJobs(ctx context.Context, staleAfter time.Duration, maxAttempts int) (int, error) {
	// Self-executing stores own their own recovery — mirrors SubmitAsync's
	// identical type-assertion guard (service.go), which skips the in-process
	// execution goroutine for the same reason: calling SaveResults,
	// UpdateJobStatus or ClearResults on one of these stores is an error, and
	// their jobs never receive an engine heartbeat, so ClaimStale's
	// COALESCE(heartbeat_time, created_at) baseline would make every healthy
	// job of theirs look stale by construction. Fail closed: skip entirely.
	if _, ok := s.searchStore.(spi.SelfExecutingSearchStore); ok {
		return 0, nil
	}

	headroom := s.asyncPool().Cap() - s.registrySize()
	if headroom <= 0 {
		return 0, nil
	}
	limit := StaleClaimBatch
	if headroom < limit {
		limit = headroom
	}

	jobs, err := s.searchStore.ClaimStale(ctx, staleAfter, limit)
	if err != nil {
		return 0, fmt.Errorf("failed to claim stale search jobs: %w", err)
	}

	reenqueued := 0
	owed := make(map[owedKey]owedWrite)
	for _, job := range jobs {
		key := owedKey{tenant: job.TenantID, jobID: job.ID}
		// Attempt cap: bound on executor losses, not on graceful handoffs.
		if int64(maxAttempts) <= job.StaleClaims {
			owed[key] = func(ctx context.Context) bool { return s.failAttemptsExhausted(ctx, job) }
			continue
		}
		started, unrun := s.reenqueueClaimed(job)
		if started {
			reenqueued++
			continue
		}
		owed[key] = func(ctx context.Context) bool { unrun(ctx); return false }
	}
	s.handOverOwed(ctx, owed)
	return reenqueued, nil
}

// owedKey names a job across tenants: a job id is unique only within its
// tenant.
type owedKey struct {
	tenant spi.TenantID
	jobID  string
}

// owedWrite is a write the reclaim sweep owes a job it claimed but will not
// run. It reports whether it failed the job at the attempt cap.
type owedWrite func(ctx context.Context) bool

// handOverOwed gives owed to the second pass, and starts it under ctx unless
// one is already in flight. A job owed a write by an earlier sweep and claimed
// again since has only its latest claim's write kept: the earlier one is at a
// superseded epoch, which the store would refuse.
func (s *SearchService) handOverOwed(ctx context.Context, owed map[owedKey]owedWrite) {
	if len(owed) == 0 {
		return
	}
	start := func() bool {
		s.secondPassMu.Lock()
		defer s.secondPassMu.Unlock()
		if s.owed == nil {
			s.owed = make(map[owedKey]owedWrite, len(owed))
		}
		for k, w := range owed {
			s.owed[k] = w
		}
		if s.secondPassDone != nil {
			return false
		}
		s.secondPassDone = make(chan struct{})
		return true
	}()
	if start {
		go s.runSecondPass(ctx)
	}
}

// takeOwed hands the second pass everything owed so far. With nothing owed it
// ends the pass, in the same lock hold, so a sweep's hand-over either lands
// before the pass ends or starts a new one.
func (s *SearchService) takeOwed() []owedWrite {
	s.secondPassMu.Lock()
	defer s.secondPassMu.Unlock()
	if len(s.owed) == 0 {
		close(s.secondPassDone)
		s.secondPassDone = nil
		return nil
	}
	writes := make([]owedWrite, 0, len(s.owed))
	for _, w := range s.owed {
		writes = append(writes, w)
	}
	clear(s.owed)
	return writes
}

// endSecondPassAfterPanic ends the pass without sending what is still owed.
// The next sweep's hand-over starts a new pass that sends it.
func (s *SearchService) endSecondPassAfterPanic() {
	s.secondPassMu.Lock()
	defer s.secondPassMu.Unlock()
	close(s.secondPassDone)
	s.secondPassDone = nil
}

// runSecondPass sends the owed writes until none is left. Once ctx ends it
// sends nothing more: the jobs whose writes it drops stay RUNNING at their
// claimed epoch and are claimed again once stale.
func (s *SearchService) runSecondPass(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic recovered in the reclaim sweep's second pass", "pkg", "search",
				"err", fmt.Errorf("panic: %v", rec), "stack", string(debug.Stack()))
			// Same latch as the executor: the same store code ran here.
			if s.healthFlag != nil {
				s.healthFlag.Store(false)
			}
			s.endSecondPassAfterPanic()
		}
	}()
	for {
		writes := s.takeOwed()
		if writes == nil {
			return
		}
		failed, dropped := 0, 0
		for _, write := range writes {
			if ctx.Err() != nil {
				dropped++
				continue
			}
			if write(ctx) {
				failed++
			}
		}
		if failed > 0 {
			slog.Warn("failed async search jobs past the attempt cap", "pkg", "search", "count", failed)
		}
		if dropped > 0 {
			slog.Info("reclaim sweep stopped with writes unsent; their jobs are claimed again once stale", "pkg", "search", "count", dropped)
		}
	}
}

// WaitReclaimSecondPass returns once the reclaim sweep's second pass, if one
// is in flight, has returned. Call it after the sweeps have stopped and the
// context they ran under has ended; then the pass sends nothing more and
// returns promptly, and no sweep can start another.
func (s *SearchService) WaitReclaimSecondPass() {
	done := func() chan struct{} {
		s.secondPassMu.Lock()
		defer s.secondPassMu.Unlock()
		return s.secondPassDone
	}()
	if done != nil {
		<-done
	}
}

// owedWriteFailed logs a second-pass write that did not land. One cut short by
// the sweep's stop is an ordinary end, not a fault.
func owedWriteFailed(ctx context.Context, level slog.Level, msg string, job *spi.SearchJob, err error) {
	if ctx.Err() != nil {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, msg, "pkg", "search", "jobID", job.ID, "err", err)
}

// ownerCtx is ctx carrying the job's tenant as the system principal.
func ownerCtx(ctx context.Context, job *spi.SearchJob) context.Context {
	return spi.WithUserContext(ctx, common.SystemUserContextValue(job.TenantID))
}

// failAttemptsExhausted fails a claimed job that has reached the attempt cap,
// at its claimed epoch, and reports whether the write landed.
func (s *SearchService) failAttemptsExhausted(ctx context.Context, job *spi.SearchJob) bool {
	if werr := s.searchStore.UpdateJobStatus(ownerCtx(ctx, job), job.ID, job.Epoch, "FAILED", 0, jobAttemptsExhausted, time.Now(), 0); werr != nil {
		if errors.Is(werr, spi.ErrAlreadyTerminal) || errors.Is(werr, spi.ErrStaleClaim) {
			slog.Warn("attempt-cap fail lost the race; job already settled", "pkg", "search", "jobID", job.ID, "err", werr)
			return false
		}
		owedWriteFailed(ctx, slog.LevelError, "failed to fail crash-looping search job", job, werr)
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
// settles it, for the caller to hand to the second pass: it fails the job (a
// genuine defect — an undecodable stored job) or releases it (transient —
// queue full). A job is never left silently RUNNING with no executor.
func (s *SearchService) reenqueueClaimed(job *spi.SearchJob) (started bool, unrun func(context.Context)) {
	uc := common.SystemUserContextValue(job.TenantID) // *spi.UserContext for the job's tenant
	baseCtx := spi.WithUserContext(context.Background(), uc)
	if scoper, ok := s.searchStore.(asyncScanScoper); ok {
		baseCtx = scoper.AsyncScanContext(baseCtx)
	}

	cond, opts, orderBy, decErr := decodeStoredJob(job)
	if decErr != nil {
		// A stored job that cannot be decoded is a defect, not a runtime
		// condition: fail it at the claimed epoch rather than loop on it.
		slog.Error("failed to decode claimed search job; failing", "pkg", "search", "jobID", job.ID, "err", decErr)
		return false, func(ctx context.Context) {
			if werr := s.searchStore.UpdateJobStatus(ownerCtx(ctx, job), job.ID, job.Epoch, "FAILED", 0, jobFailureFallback, time.Now(), 0); werr != nil {
				owedWriteFailed(ctx, slog.LevelError, "failed to fail undecodable claimed job", job, werr)
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
		return false, func(ctx context.Context) { s.releaseUnrun(ctx, job) }
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
			s.releaseUnrun(context.Background(), job)
			return false
		}
	}
	cause := context.Cause(jobCtx)
	slog.Debug("reclaimed job ended before it ran", "pkg", "search", "jobID", job.ID, "cause", cause)
	s.dropReclaim(cancel, handle, job)
	if !errors.Is(cause, errJobReleased) && !errors.Is(cause, errJobSuperseded) {
		s.releaseUnrun(context.Background(), job)
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
func (s *SearchService) releaseUnrun(ctx context.Context, job *spi.SearchJob) {
	rerr := s.searchStore.Release(ownerCtx(ctx, job), job.ID, job.Epoch)
	switch {
	case rerr == nil:
	case errors.Is(rerr, spi.ErrStaleClaim), errors.Is(rerr, spi.ErrAlreadyTerminal), errors.Is(rerr, spi.ErrNotFound):
		slog.Debug("reclaimed job that did not run is no longer this epoch's", "pkg", "search", "jobID", job.ID, "err", rerr)
	default:
		owedWriteFailed(ctx, slog.LevelWarn, "failed to release reclaimed job that did not run", job, rerr)
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
