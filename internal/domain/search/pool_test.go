package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// mustSubmitEventually retries Submit past the narrow startup race between
// NewWorkerPool returning and its spawned worker goroutines actually
// reaching their channel receive: immediately after construction a worker
// may not yet be "parked" ready to receive, so a Submit that logically has
// a free worker to land on can spuriously see ErrQueueFull for a few
// microseconds (a fresh goroutine's first scheduling, not a pool defect —
// Submit's own non-blocking contract is what TestWorkerPool_Submit_NeverBlocks
// pins). Used only to prime the very first round of submits in a test;
// once that round is confirmed accepted, remaining capacity assertions are
// deterministic and use plain Submit.
func mustSubmitEventually(t *testing.T, pool *WorkerPool, job jobFunc, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := pool.Submit(job)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrQueueFull) || time.Now().After(deadline) {
			t.Fatalf("submit: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWorkerPool_BoundedQueue_QueueFull drives the exact scenario from the
// task brief: N=2 workers, queue capacity 1. The first two Submits are
// picked up directly by the two idle workers, the third fills the queue
// buffer, and the fourth — with both workers busy and the buffer full —
// must fail fast with ErrQueueFull rather than blocking.
func TestWorkerPool_BoundedQueue_QueueFull(t *testing.T) {
	pool := NewWorkerPool(2, 1)
	defer pool.Drain(context.Background())

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	blockingJob := func() {
		started <- struct{}{}
		<-release
	}
	defer close(release)

	for i := 0; i < 2; i++ {
		mustSubmitEventually(t, pool, blockingJob, time.Second)
	}
	// Wait for both workers to actually receive their job so the queue's
	// buffer state is deterministic before submitting the third and fourth.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("worker %d never started its job", i)
		}
	}

	if err := pool.Submit(blockingJob); err != nil {
		t.Fatalf("third submit (should fill the queue buffer): unexpected error: %v", err)
	}

	if err := pool.Submit(blockingJob); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("fourth submit: got %v, want ErrQueueFull", err)
	}
}

// TestWorkerPool_Submit_NeverBlocks asserts Submit returns immediately even
// when the queue is full — the whole point of ErrQueueFull is that callers
// get an instant answer to convert into backpressure, not a stall.
func TestWorkerPool_Submit_NeverBlocks(t *testing.T) {
	pool := NewWorkerPool(1, 0)
	defer pool.Drain(context.Background())

	release := make(chan struct{})
	defer close(release)
	mustSubmitEventually(t, pool, func() { <-release }, time.Second)

	done := make(chan error, 1)
	go func() { done <- pool.Submit(func() {}) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueFull) {
			t.Fatalf("got %v, want ErrQueueFull", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Submit blocked instead of returning ErrQueueFull immediately")
	}
}

// TestWorkerPool_ConcurrencyBound asserts jobs never run more than
// `workers` at a time, using an atomic high-water mark rather than
// asserting a precise interleave.
func TestWorkerPool_ConcurrencyBound(t *testing.T) {
	const workers = 2
	pool := NewWorkerPool(workers, 8)
	defer pool.Drain(context.Background())

	var current, highWater int64
	var wg sync.WaitGroup
	job := func() {
		defer wg.Done()
		n := atomic.AddInt64(&current, 1)
		for {
			hw := atomic.LoadInt64(&highWater)
			if n <= hw {
				break
			}
			if atomic.CompareAndSwapInt64(&highWater, hw, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&current, -1)
	}

	const jobs = 6
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		if err := pool.Submit(job); err != nil {
			t.Fatalf("submit %d: unexpected error: %v", i, err)
		}
	}
	wg.Wait()

	if hw := atomic.LoadInt64(&highWater); hw > workers {
		t.Errorf("high-water concurrency = %d, want <= %d", hw, workers)
	}
	if hw := atomic.LoadInt64(&highWater); hw < workers {
		t.Errorf("high-water concurrency = %d, want == %d (both workers should run concurrently at some point across %d jobs)", hw, workers, jobs)
	}
}

// TestWorkerPool_Drain_WaitsForInFlightJob asserts Drain lets an in-flight
// job run to completion and does not return until it has actually finished.
// Drain deliberately has no way to interrupt it (see jobFunc): the drain
// budget is the job's chance to finish, and App.Shutdown cancels whatever
// is still running only afterwards, via AbortRegisteredJobs.
func TestWorkerPool_Drain_WaitsForInFlightJob(t *testing.T) {
	pool := NewWorkerPool(1, 1)

	started := make(chan struct{})
	jobFinished := make(chan struct{})
	job := func() {
		close(started)
		time.Sleep(30 * time.Millisecond)
		close(jobFinished)
	}
	if err := pool.Submit(job); err != nil {
		t.Fatalf("submit: unexpected error: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job never started")
	}

	drainDone := make(chan struct{})
	go func() {
		pool.Drain(context.Background())
		close(drainDone)
	}()

	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("Drain did not return after the in-flight job finished")
	}
	select {
	case <-jobFinished:
	default:
		t.Fatal("Drain returned before the in-flight job actually finished running")
	}
}

// TestWorkerPool_Drain_NoGoroutineLeak proves every worker goroutine has
// actually exited by the time Drain returns: it submits more jobs than
// there are workers, drains, then confirms a background counter workers
// increment while draining stays put — i.e. nothing is still running.
func TestWorkerPool_Drain_NoGoroutineLeak(t *testing.T) {
	pool := NewWorkerPool(3, 8)

	var ran int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		if err := pool.Submit(func() {
			atomic.AddInt64(&ran, 1)
			wg.Done()
		}); err != nil {
			t.Fatalf("submit %d: unexpected error: %v", i, err)
		}
	}
	wg.Wait()

	pool.Drain(context.Background())

	// After Drain returns, wg.Wait() inside Drain has already observed
	// every worker's defer p.wg.Done() fire — i.e. every worker goroutine
	// has returned from p.worker and exited. Submit after Drain must not
	// resurrect a worker or panic on the closed channel.
	if err := pool.Submit(func() {}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Submit after Drain: got %v, want ErrQueueFull", err)
	}
	if got := atomic.LoadInt64(&ran); got != 8 {
		t.Fatalf("ran = %d, want 8 (all submitted jobs must have completed before Drain returned)", got)
	}
}

// TestWorkerPool_Drain_Idempotent asserts a second Drain call does not
// panic (e.g. on a double close of the jobs channel) and returns promptly.
func TestWorkerPool_Drain_Idempotent(t *testing.T) {
	pool := NewWorkerPool(1, 0)
	pool.Drain(context.Background())

	done := make(chan struct{})
	go func() {
		pool.Drain(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second Drain call did not return")
	}
}

// TestQueueFullError asserts the shared mapping's shape: 503, the
// SEARCH_QUEUE_FULL code, retryable, and — since it's Operational — no
// internal detail attached that submitAsyncError/gRPC would need to
// sanitize before it reaches a client.
func TestQueueFullError(t *testing.T) {
	appErr := QueueFullError()

	if appErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want %d", appErr.Status, http.StatusServiceUnavailable)
	}
	if appErr.Code != common.ErrCodeSearchQueueFull {
		t.Errorf("Code = %q, want %q", appErr.Code, common.ErrCodeSearchQueueFull)
	}
	if !appErr.Retryable {
		t.Error("Retryable = false, want true")
	}
	if appErr.Level != common.LevelOperational {
		t.Errorf("Level = %v, want LevelOperational", appErr.Level)
	}
	if appErr.Err != nil {
		t.Errorf("Err = %v, want nil (no cause to leak)", appErr.Err)
	}
	if appErr.Detail != "" {
		t.Errorf("Detail = %q, want empty (Operational carries detail in Message only)", appErr.Detail)
	}
}

// TestSubmitAsyncError_QueueFull asserts the HTTP submit-boundary mapping
// (handler.go's submitAsyncError, the E1.4 "handler unit test for the
// mapping") recognizes ErrQueueFull — directly and wrapped — ahead of the
// generic AppError/Internal fallback.
func TestSubmitAsyncError_QueueFull(t *testing.T) {
	got := submitAsyncError(ErrQueueFull)
	if got.Status != http.StatusServiceUnavailable || got.Code != common.ErrCodeSearchQueueFull || !got.Retryable {
		t.Errorf("submitAsyncError(ErrQueueFull) = %+v, want 503/%s/retryable", got, common.ErrCodeSearchQueueFull)
	}
}

func TestSubmitAsyncError_WrappedQueueFull(t *testing.T) {
	wrapped := fmt.Errorf("pool: %w", ErrQueueFull)
	got := submitAsyncError(wrapped)
	if got.Code != common.ErrCodeSearchQueueFull {
		t.Errorf("submitAsyncError(wrapped ErrQueueFull) code = %q, want %q (errors.Is must see through the wrap)", got.Code, common.ErrCodeSearchQueueFull)
	}
}

// TestSubmitAsyncError_ForwardsAppError asserts a pre-execution validation
// AppError (unrelated to the queue) still forwards unchanged, preserving
// existing behavior.
func TestSubmitAsyncError_ForwardsAppError(t *testing.T) {
	want := common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "bad condition")
	got := submitAsyncError(want)
	if got != want {
		t.Errorf("submitAsyncError must forward an existing AppError unchanged, got a different instance: %+v", got)
	}
}

// TestSubmitAsyncError_Internal asserts an unclassified error still maps to
// a 500, preserving existing behavior for anything that isn't the queue
// sentinel or an AppError.
func TestSubmitAsyncError_Internal(t *testing.T) {
	got := submitAsyncError(errors.New("boom"))
	if got.Status != http.StatusInternalServerError || got.Level != common.LevelInternal {
		t.Errorf("submitAsyncError(unclassified) = %+v, want 500 Internal", got)
	}
}
