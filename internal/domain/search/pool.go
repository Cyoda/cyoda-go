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

// jobFunc is a unit of work a WorkerPool runs. It receives the pool's
// lifetime context, which is cancelled when Drain runs — a long-running job
// must select on ctx.Done() to stop promptly instead of running to
// completion regardless.
type jobFunc func(ctx context.Context)

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
type WorkerPool struct {
	mu     sync.RWMutex // guards closed; see Submit/Drain
	closed bool
	jobs   chan jobFunc
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorkerPool starts n workers draining a queue of capacity qlen and
// returns immediately; the workers run until Drain is called. Callers must
// validate workers >= 1 and qlen >= 0 before calling — this constructor
// does not, so a misconfigured value fails visibly at the validation step
// (app.ValidateSearchAsync) rather than silently here.
func NewWorkerPool(workers, queueLen int) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &WorkerPool{
		jobs:   make(chan jobFunc, queueLen),
		cancel: cancel,
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker(ctx)
	}
	return p
}

func (p *WorkerPool) worker(ctx context.Context) {
	defer p.wg.Done()
	for job := range p.jobs {
		job(ctx)
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

// Drain stops intake, cancels the pool's lifetime context (observable by
// every in-flight job via its ctx argument), and waits for every worker to
// finish its current job and exit — or for ctx to expire, whichever comes
// first. After Drain returns with every worker exited, no pool goroutines
// remain running. Idempotent: a second Drain call is a no-op. Submit calls
// racing a concurrent Drain observe ErrQueueFull instead of panicking on a
// closed channel.
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

	p.cancel()

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
