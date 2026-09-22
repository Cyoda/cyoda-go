package txgate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistry_SerialisesSameTxID(t *testing.T) {
	r := New()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	rel1 := r.Acquire("tx-1")
	wg.Add(1)
	go func() {
		defer wg.Done()
		rel := r.Acquire("tx-1") // must block until rel1() runs
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		rel()
	}()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	order = append(order, 1)
	mu.Unlock()
	rel1()
	wg.Wait()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("expected serialized order [1 2], got %v", order)
	}
}

func TestRegistry_DifferentTxIDsDoNotBlock(t *testing.T) {
	r := New()
	rel1 := r.Acquire("tx-1")
	done := make(chan struct{})
	go func() { rel2 := r.Acquire("tx-2"); rel2(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire on a different txID blocked")
	}
	rel1()
}

// A waiter whose own context ends gives the wait up: it holds nothing, it is
// told why, and it leaves no entry behind. Nothing of the transaction has been
// touched before the gate is held, so there is nothing to undo.
func TestRegistry_AcquireCtx_AWaiterWhoseContextEndsGivesUp(t *testing.T) {
	r := New()
	holder := r.Acquire("tx-1")
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		release, err := r.AcquireCtx(ctx, "tx-1", 0)
		if release != nil {
			t.Error("a waiter that gave up was handed a release func")
		}
		gaveUp <- err
	}()

	select {
	case err := <-gaveUp:
		t.Fatalf("AcquireCtx returned %v while the gate was held", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a waiter whose context ended did not return")
	}

	holder()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries; a waiter that gave up left one behind", n)
	}
}

// Once the gate is held, the holder's context no longer bears on it: a joined
// request whose compute node goes away mid-statement keeps the gate until it
// releases it.
func TestRegistry_AcquireCtx_OnceHeldTheContextDoesNotMatter(t *testing.T) {
	r := New()
	ctx, cancel := context.WithCancel(context.Background())
	release, err := r.AcquireCtx(ctx, "tx-1", 0)
	if err != nil {
		t.Fatalf("AcquireCtx: %v", err)
	}
	cancel()

	free := make(chan struct{})
	go func() { r.Acquire("tx-1")(); close(free) }()
	select {
	case <-free:
		t.Fatal("the gate was given up when the holder's context ended")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case <-free:
	case <-time.After(time.Second):
		t.Fatal("the gate was not released")
	}
}

// A context that has already ended never takes the gate, even when it is free:
// the caller is gone, so its request must not start.
func TestRegistry_AcquireCtx_ContextAlreadyEnded_TakesNothing(t *testing.T) {
	r := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := r.AcquireCtx(ctx, "tx-1", 0); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatalf("AcquireCtx handed out a gate (%v) or the wrong error: %v", release != nil, err)
	}
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

func TestRegistry_ReleasesMapEntry(t *testing.T) {
	r := New()
	r.Acquire("tx-1")() // acquire+release
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after release, got %d", n)
	}
}

// TestSuspend_NoHeldGate_NoOp: a ctx with no held gate installed (the owner
// path, or a plain non-joined call) yields a no-op Suspend whose resume is also
// a harmless no-op — it must never touch any gate.
func TestSuspend_NoHeldGate_NoOp(t *testing.T) {
	resume := Suspend(context.Background())
	if resume == nil {
		t.Fatal("Suspend returned a nil resume for a ctx with no held gate")
	}
	resume() // must not panic
}

// TestSuspend_ReleasesHeldGate_ResumeReacquires encodes the fix's core seam.
// A joined caller holds gate(T) and installs a held handle on ctx via WithHeld.
// While it is "parked in dispatch" it calls Suspend(ctx): the gate MUST be
// released so a descendant on the same txID can Acquire and progress. After the
// descendant releases, resume() MUST re-acquire the gate before the caller
// resumes its buffer access — and the caller's own release variable must observe
// the fresh release func so the deferred release matches the re-acquire.
func TestSuspend_ReleasesHeldGate_ResumeReacquires(t *testing.T) {
	r := New()
	const txID = "tx-suspend"

	// Joined caller acquires the gate and records it on ctx.
	release := r.Acquire(txID)
	ctx, _ := WithHeld(context.Background(), r, txID, &release)

	// Descendant tries to acquire the SAME gate. It must block until Suspend
	// releases, then complete, then release before resume can re-acquire.
	descendantAcquired := make(chan struct{})
	descendantReleased := make(chan struct{})
	go func() {
		rel := r.Acquire(txID) // blocks until the caller Suspends
		close(descendantAcquired)
		time.Sleep(20 * time.Millisecond) // hold briefly so resume must wait
		rel()
		close(descendantReleased)
	}()

	// Before Suspend the descendant must NOT be able to acquire.
	select {
	case <-descendantAcquired:
		t.Fatal("descendant acquired gate(T) while the caller still held it — Suspend not yet called")
	case <-time.After(30 * time.Millisecond):
	}

	resume := Suspend(ctx)

	// Now the descendant progresses (the gate was released).
	select {
	case <-descendantAcquired:
	case <-time.After(time.Second):
		t.Fatal("descendant could not acquire gate(T) after Suspend — the held gate was not released")
	}

	// resume() must block until the descendant releases, then re-acquire.
	resume()
	select {
	case <-descendantReleased:
	case <-time.After(time.Second):
		t.Fatal("resume() returned before the descendant released — it did not re-acquire the gate")
	}

	// The caller's release variable now points at the re-acquired gate; calling
	// it must free the gate (a fresh Acquire must not block afterward).
	release()
	done := make(chan struct{})
	go func() { r.Acquire(txID)(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gate still held after the caller's release() — resume did not rebind the release func")
	}
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after all releases, got %d", n)
	}
}

func TestRegistry_EmptyTxID_Noop(t *testing.T) {
	r := New()
	// Acquire("") must return a callable no-op immediately without adding a map entry.
	rel := r.Acquire("")
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map for empty txID, got %d", n)
	}
	rel() // must not panic
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after release of empty txID, got %d", n)
	}
}

// waitFor polls cond until it holds. The queue on a gate is built by other
// goroutines, so a test that has to see a caller waiting waits for it rather
// than sleeping a guessed interval.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A transaction's gate takes a bounded queue. Past the cap AcquireCtx refuses
// instead of parking another caller: a compute member that fires callbacks
// faster than they are served would otherwise hold a request's worth of memory
// per callback for the life of the callout.
func TestRegistry_AcquireCtx_RefusesPastTheWaiterCap(t *testing.T) {
	r := New()
	holder := r.Acquire("tx-1")

	queued := make(chan func(), 1)
	go func() {
		release, err := r.AcquireCtx(context.Background(), "tx-1", 1)
		if err != nil {
			t.Errorf("the one waiter a cap of 1 admits was refused: %v", err)
			return
		}
		queued <- release
	}()
	waitFor(t, "the first waiter to reach the gate", func() bool { return r.AtCapacity("tx-1", 1) })

	release, err := r.AcquireCtx(context.Background(), "tx-1", 1)
	if !errors.Is(err, ErrTooManyWaiters) {
		t.Fatalf("AcquireCtx err = %v; want ErrTooManyWaiters", err)
	}
	if release != nil {
		t.Fatal("a refused caller was handed a gate")
	}

	// The holder and the waiter already queued are untouched: the queue drains.
	holder()
	select {
	case rel := <-queued:
		rel()
	case <-time.After(5 * time.Second):
		t.Fatal("the queued waiter never got the gate")
	}
	if n := r.len(); n != 0 {
		t.Errorf("gate map = %d entries; a refusal must leave nothing behind", n)
	}
}

// The count is of callers waiting, not of the holder: a cap of n admits the
// holder plus n queued.
func TestRegistry_AcquireCtx_TheHolderIsNotAWaiter(t *testing.T) {
	r := New()
	release, err := r.AcquireCtx(context.Background(), "tx-1", 1)
	if err != nil {
		t.Fatalf("the first caller of a free gate was refused: %v", err)
	}
	defer release()
	if r.AtCapacity("tx-1", 1) {
		t.Error("a gate with a holder and nobody queued reports itself full")
	}
}

// Acquire is the wait the transaction owner's own chain, the fence and a
// suspended callback's resume take, and none of them may be refused: each holds
// state a refusal could not undo. The cap belongs to the joined doors alone.
func TestRegistry_Acquire_IsNeverRefusedForCapacity(t *testing.T) {
	r := New()
	holder := r.Acquire("tx-1")
	go func() {
		if release, err := r.AcquireCtx(context.Background(), "tx-1", 1); err == nil {
			release()
		}
	}()
	waitFor(t, "the gate to fill", func() bool { return r.AtCapacity("tx-1", 1) })

	got := make(chan struct{})
	go func() { r.Acquire("tx-1")(); close(got) }()
	holder()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("an uncapped wait never got the gate")
	}
}

// A transaction nothing is queued for is never at capacity, whether it has a
// gate or has never had one — the two read the same, which is what keeps the
// reading from saying whether a transaction exists.
func TestRegistry_AtCapacity_NothingQueued(t *testing.T) {
	r := New()
	if r.AtCapacity("tx-never-seen", 1) {
		t.Error("a transaction with no gate reports itself full")
	}
	release := r.Acquire("tx-1")
	defer release()
	if r.AtCapacity("tx-1", 1) {
		t.Error("a gate nobody is queued for reports itself full")
	}
}

// An empty txID is not a joined request and was never gated, so no cap bears on
// it.
func TestRegistry_AcquireCtx_EmptyTxID_IsNotCapped(t *testing.T) {
	r := New()
	release, err := r.AcquireCtx(context.Background(), "", 1)
	if err != nil || release == nil {
		t.Fatalf(`AcquireCtx("") = %v, %v; want a no-op release`, release != nil, err)
	}
	release()
	if r.AtCapacity("", 1) {
		t.Error("the empty txID reports itself full")
	}
}
