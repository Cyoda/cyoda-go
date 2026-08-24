package search

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// ErrQueueFull is returned by WorkerPool.Submit when the pool's queue is at
// capacity. Submit never blocks to wait for room — the caller decides how
// to respond to backpressure (the async-search submit boundary maps this to
// a retryable 503; see QueueFullError).
var ErrQueueFull = errors.New("async search queue is full")

// jobFunc is a unit of work a WorkerPool runs.
//
// It takes no context on purpose. The pool used to hand each job a lifetime
// context cancelled by Drain, but no submitter ever observed it: the async
// executor runs on its own per-job context (the one the cancel registry and
// the heartbeat ticker share), and cancelling in-flight jobs the moment
// Drain starts would defeat what Drain is for — App.Shutdown drains first,
// giving a job the chance to finish inside the drain budget, and only then
// calls AbortRegisteredJobs to cancel whatever is left. A parameter no
// caller can act on, documented as a cancellation promise the pool does not
// keep, is worse than none.
type jobFunc func()

// WorkerPool is a bounded pool of goroutines draining a fixed-capacity
// queue of jobFunc. It backs the async-search submit path: SubmitAsync
// hands each search job to Submit instead of running it inline, so a burst
// of submissions is bounded by (workers running + queue depth) rather than
// spawning unbounded goroutines.
//
// Sizing (CYODA_SEARCH_ASYNC_WORKERS default 8, CYODA_SEARCH_ASYNC_QUEUE
// default 256, see app.SearchAsyncConfig): a streaming search job holds its
// scan connection for the run's duration plus, per chunk, a save
// connection — so the worker count is bounded by the postgres connection
// budget: 8 workers <= (default 25 max conns - reserve) / 2 connections
// held at a job's peak.
//
// The pool is tenant-blind by design. Fairness between tenants is enforced
// one level up, at the submit boundary, by SearchService's per-tenant
// in-flight cap (WithAsyncMaxPerTenant).
type WorkerPool struct {
	mu     sync.RWMutex // guards closed; see Submit/Drain
	closed bool
	jobs   chan jobFunc
	wg     sync.WaitGroup
}

// NewWorkerPool starts n workers draining a queue of capacity qlen and
// returns immediately; the workers run until Drain is called. Callers must
// validate workers >= 1 and qlen >= 0 before calling — this constructor
// does not, so a misconfigured value fails visibly at the validation step
// (app.ValidateSearchAsync) rather than silently here.
func NewWorkerPool(workers, queueLen int) *WorkerPool {
	p := &WorkerPool{jobs: make(chan jobFunc, queueLen)}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *WorkerPool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		job()
	}
}

// Submit enqueues job for execution by a worker. It never blocks: if every
// worker is busy and the queue is at capacity, it returns ErrQueueFull
// immediately rather than waiting for room.
func (p *WorkerPool) Submit(job jobFunc) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrQueueFull
	}
	select {
	case p.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// Drain stops intake and waits for every worker to finish its current job
// and exit — or for ctx to expire, whichever comes first. It does NOT
// cancel in-flight jobs: giving them the drain budget to finish is the
// point (see jobFunc), and App.Shutdown cancels whatever is still running
// afterwards via AbortRegisteredJobs. After Drain returns with every worker
// exited, no pool goroutines remain running. Idempotent: a second Drain
// call is a no-op. Submit calls racing a concurrent Drain observe
// ErrQueueFull instead of panicking on a closed channel.
func (p *WorkerPool) Drain(ctx context.Context) {
	alreadyClosed := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return true
		}
		p.closed = true
		close(p.jobs)
		return false
	}()
	if alreadyClosed {
		return
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// QueueFullError returns the retryable 503 *common.AppError for
// ErrQueueFull. Defined once here and reused by every transport (HTTP via
// submitAsyncError in handler.go, gRPC via internal/grpc/search.go) so the
// status/code/message/retryable quadruple for SEARCH_QUEUE_FULL has a
// single source of truth.
func QueueFullError() *common.AppError {
	return common.Operational(
		http.StatusServiceUnavailable,
		common.ErrCodeSearchQueueFull,
		"async search queue is full — retry later",
	).AsRetryable()
}
