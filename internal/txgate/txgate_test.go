package txgate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func always() bool { return true }

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

// queued reports how many callers wait on txID's gate.
func (r *Registry) queued(txID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[txID]
	if g == nil {
		return 0
	}
	return len(g.queue)
}

// mustAcquire takes the slot and fails the test on a refusal.
func mustAcquire(t *testing.T, r *Registry, txID string) func() {
	t.Helper()
	release, err := r.AcquireJoined(context.Background(), txID, 0, always)
	if err != nil {
		t.Fatalf("AcquireJoined(%q): %v", txID, err)
	}
	return release
}

// expectNotYet fails when ch delivers within a short window: the caller behind
// it must still be waiting.
func expectNotYet[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s before it should have", what)
	case <-time.After(30 * time.Millisecond):
	}
}

func expectSoon[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

type result struct {
	release func()
	err     error
}

func acquireAsync(r *Registry, ctx context.Context, txID string, maxWaiters int, admitted func() bool) <-chan result {
	ch := make(chan result, 1)
	go func() {
		release, err := r.AcquireJoined(ctx, txID, maxWaiters, admitted)
		ch <- result{release, err}
	}()
	return ch
}

func TestRegistry_SerialisesSameTxID(t *testing.T) {
	r := New()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	rel1 := mustAcquire(t, r, "tx-1")
	wg.Add(1)
	go func() {
		defer wg.Done()
		rel := r.AcquireResume("tx-1") // must block until rel1() runs
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
	rel1 := mustAcquire(t, r, "tx-1")
	done := make(chan struct{})
	go func() { r.AcquireResume("tx-2")(); close(done) }()
	expectSoon(t, done, "a gate on a different txID")
	rel1()
}

// The slot passes to the queued callers in the order they queued.
func TestRegistry_GrantsInQueueOrder(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")
	const n = 5
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := r.AcquireJoined(context.Background(), "tx-1", 0, always)
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			release()
		}()
		waitFor(t, "the waiter to queue", func() bool { return r.queued("tx-1") == i+1 })
	}
	holder()
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("grant order = %v; want queue order", order)
		}
	}
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

func TestRegistry_ReleasesMapEntry(t *testing.T) {
	r := New()
	mustAcquire(t, r, "tx-1")()
	r.AcquireResume("tx-1")()
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after release, got %d", n)
	}
}

// A release func runs once: the suspend handle and the caller's own defer may
// both call it, and the second call must not hand the slot out again.
func TestRegistry_ReleaseIsIdempotent(t *testing.T) {
	r := New()
	release := mustAcquire(t, r, "tx-1")
	second := acquireAsync(r, context.Background(), "tx-1", 0, always)
	waitFor(t, "the second caller to queue", func() bool { return r.queued("tx-1") == 1 })
	third := acquireAsync(r, context.Background(), "tx-1", 0, always)
	waitFor(t, "the third caller to queue", func() bool { return r.queued("tx-1") == 2 })

	release()
	release()
	got := expectSoon(t, second, "the second caller to take the slot")
	expectNotYet(t, third, "the third caller took the slot while the second held it")
	got.release()
	expectSoon(t, third, "the third caller to take the slot").release()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

func TestRegistry_EmptyTxID_Noop(t *testing.T) {
	r := New()
	release, err := r.AcquireJoined(context.Background(), "", 1, func() bool { return false })
	if err != nil || release == nil {
		t.Fatalf(`AcquireJoined("") = %v, %v; want a no-op release`, release != nil, err)
	}
	release()
	r.AcquireResume("")()
	r.Reevaluate("")
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map for empty txID, got %d", n)
	}
	if r.AtCapacity("", 1) {
		t.Error("the empty txID reports itself full")
	}
}

// --- admission -------------------------------------------------------------

// A caller the gate does not admit is refused at once, holding nothing and
// leaving nothing queued — whether or not the slot is free.
func TestAcquireJoined_NotAdmittedOnEntry(t *testing.T) {
	r := New()
	refuse := func() bool { return false }

	release, err := r.AcquireJoined(context.Background(), "tx-1", 1, refuse)
	if !errors.Is(err, ErrNotAdmitted) || release != nil {
		t.Fatalf("free gate: AcquireJoined = %v, %v; want ErrNotAdmitted and no release", release != nil, err)
	}
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries after a refusal", n)
	}

	holder := mustAcquire(t, r, "tx-1")
	release, err = r.AcquireJoined(context.Background(), "tx-1", 1, refuse)
	if !errors.Is(err, ErrNotAdmitted) || release != nil {
		t.Fatalf("held gate: AcquireJoined = %v, %v; want ErrNotAdmitted and no release", release != nil, err)
	}
	if r.AtCapacity("tx-1", 1) {
		t.Error("a refused caller is counted as queued")
	}
	holder()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

// Admission is read before the cap: a caller the gate does not admit is told
// so, not that the queue is full.
func TestAcquireJoined_NotAdmittedBeforeCap(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")
	queued := acquireAsync(r, context.Background(), "tx-1", 1, always)
	waitFor(t, "the queue to fill", func() bool { return r.AtCapacity("tx-1", 1) })

	if _, err := r.AcquireJoined(context.Background(), "tx-1", 1, func() bool { return false }); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("err = %v; want ErrNotAdmitted", err)
	}
	holder()
	expectSoon(t, queued, "the queued caller").release()
}

// A queued caller the gate stops admitting is refused when Reevaluate runs; the
// holder keeps the slot, and the refused caller leaves nothing behind.
func TestAcquireJoined_QueuedThenRefusedOnReevaluate(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")

	var admit atomic.Bool
	admit.Store(true)
	queued := acquireAsync(r, context.Background(), "tx-1", 0, admit.Load)
	waitFor(t, "the second caller to queue", func() bool { return r.queued("tx-1") == 1 })

	r.Reevaluate("tx-1") // still admitted: stays queued
	expectNotYet(t, queued, "a still-admitted caller returned on Reevaluate")

	admit.Store(false)
	r.Reevaluate("tx-1")
	got := expectSoon(t, queued, "the refused caller to return")
	if !errors.Is(got.err, ErrNotAdmitted) || got.release != nil {
		t.Fatalf("AcquireJoined = %v, %v; want ErrNotAdmitted and no release", got.release != nil, got.err)
	}
	if r.queued("tx-1") != 0 {
		t.Fatal("the refused caller is still queued")
	}

	// The holder still holds the slot: nobody else can take it.
	other := acquireAsync(r, context.Background(), "tx-1", 0, always)
	expectNotYet(t, other, "the slot was taken while the holder held it")
	holder()
	expectSoon(t, other, "the next caller").release()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries; a refused caller left one behind", n)
	}
}

// Reevaluate refuses only the callers no longer admitted; the others keep their
// place, and the slot passes over the refused ones to the next admitted caller.
func TestReevaluate_RefusesOnlyTheNoLongerAdmitted(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")

	var admitA atomic.Bool
	admitA.Store(true)
	a := acquireAsync(r, context.Background(), "tx-1", 0, admitA.Load)
	waitFor(t, "A to queue", func() bool { return r.queued("tx-1") == 1 })
	b := acquireAsync(r, context.Background(), "tx-1", 0, always)
	waitFor(t, "B to queue", func() bool { return r.queued("tx-1") == 2 })

	admitA.Store(false)
	r.Reevaluate("tx-1")
	if got := expectSoon(t, a, "A to be refused"); !errors.Is(got.err, ErrNotAdmitted) {
		t.Fatalf("A: err = %v; want ErrNotAdmitted", got.err)
	}
	expectNotYet(t, b, "B returned on a Reevaluate that still admits it")
	holder()
	got := expectSoon(t, b, "B to take the slot")
	if got.err != nil {
		t.Fatalf("B: %v", got.err)
	}
	got.release()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

// The grant honours admission too: a queued caller whose predicate has turned
// false is refused when the slot reaches it, even with no Reevaluate since, and
// the slot goes on to the next admitted caller.
func TestRelease_GrantRereadsAdmission(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")

	var admitA atomic.Bool
	admitA.Store(true)
	a := acquireAsync(r, context.Background(), "tx-1", 0, admitA.Load)
	waitFor(t, "A to queue", func() bool { return r.queued("tx-1") == 1 })
	b := acquireAsync(r, context.Background(), "tx-1", 0, always)
	waitFor(t, "B to queue", func() bool { return r.queued("tx-1") == 2 })

	admitA.Store(false) // no Reevaluate
	holder()

	gotA := expectSoon(t, a, "A to be refused at the grant")
	if !errors.Is(gotA.err, ErrNotAdmitted) || gotA.release != nil {
		t.Fatalf("A: AcquireJoined = %v, %v; want ErrNotAdmitted and no release", gotA.release != nil, gotA.err)
	}
	gotB := expectSoon(t, b, "B to take the slot")
	if gotB.err != nil {
		t.Fatalf("B: %v", gotB.err)
	}
	gotB.release()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

// Reevaluate on a transaction with no gate does nothing.
func TestReevaluate_NoGate(t *testing.T) {
	r := New()
	r.Reevaluate("tx-never-seen")
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

// --- resume ----------------------------------------------------------------

// A paused scope's resume is not capped and not subject to admission: it waits
// its turn in the queue, however full, and is never refused by Reevaluate.
func TestAcquireResume_IgnoresCapAndAdmission(t *testing.T) {
	r := New()
	const maxWaiters = 2
	holder := mustAcquire(t, r, "tx-1")
	var waiters []<-chan result
	for i := range maxWaiters {
		waiters = append(waiters, acquireAsync(r, context.Background(), "tx-1", maxWaiters, always))
		waitFor(t, "the waiter to queue", func() bool { return r.queued("tx-1") == i+1 })
	}
	if !r.AtCapacity("tx-1", maxWaiters) {
		t.Fatal("the queue is not full")
	}
	if _, err := r.AcquireJoined(context.Background(), "tx-1", maxWaiters, always); !errors.Is(err, ErrTooManyWaiters) {
		t.Fatalf("a joined caller past the cap: err = %v; want ErrTooManyWaiters", err)
	}

	resumed := make(chan func(), 1)
	go func() { resumed <- r.AcquireResume("tx-1") }()
	waitFor(t, "the resume to queue", func() bool { return r.queued("tx-1") == maxWaiters+1 })
	r.Reevaluate("tx-1")
	expectNotYet(t, resumed, "the resume took the slot while it was held")

	holder()
	for _, w := range waiters {
		got := expectSoon(t, w, "a queued joined caller")
		if got.err != nil {
			t.Fatalf("queued caller: %v", got.err)
		}
		got.release()
	}
	expectSoon(t, resumed, "the resume to take the slot")()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

// --- the caller's context ----------------------------------------------------

// A waiter whose own context ends gives the wait up: it holds nothing, it is
// told why, and it leaves no entry behind. Nothing of the transaction has been
// touched before the gate is held, so there is nothing to undo.
func TestAcquireJoined_CtxEndsWhileQueued(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := acquireAsync(r, ctx, "tx-1", 0, always)
	waitFor(t, "the waiter to queue", func() bool { return r.queued("tx-1") == 1 })
	expectNotYet(t, gaveUp, "AcquireJoined returned while the gate was held")

	cancel()
	got := expectSoon(t, gaveUp, "a waiter whose context ended")
	if !errors.Is(got.err, context.Canceled) || got.release != nil {
		t.Fatalf("AcquireJoined = %v, %v; want context.Canceled and no release", got.release != nil, got.err)
	}
	if r.queued("tx-1") != 0 {
		t.Fatal("a waiter that gave up is still queued")
	}

	holder()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries; a waiter that gave up left one behind", n)
	}
}

// Once the gate is held, the holder's context no longer bears on it: a joined
// request whose compute node goes away mid-statement keeps the gate until it
// releases it.
func TestAcquireJoined_OnceHeldTheContextDoesNotMatter(t *testing.T) {
	r := New()
	ctx, cancel := context.WithCancel(context.Background())
	release, err := r.AcquireJoined(ctx, "tx-1", 0, always)
	if err != nil {
		t.Fatalf("AcquireJoined: %v", err)
	}
	cancel()

	free := make(chan struct{})
	go func() { r.AcquireResume("tx-1")(); close(free) }()
	expectNotYet(t, free, "the gate was given up when the holder's context ended")
	release()
	expectSoon(t, free, "the gate to be released")
}

// A context that has already ended never takes the gate, even when it is free:
// the caller is gone, so its request must not start.
func TestAcquireJoined_ContextAlreadyEnded_TakesNothing(t *testing.T) {
	r := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := r.AcquireJoined(ctx, "tx-1", 0, always); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatalf("AcquireJoined handed out a gate (%v) or the wrong error: %v", release != nil, err)
	}
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
	}
}

// A slot granted to a waiter whose context ends at the same moment is passed on,
// never lost: however grants, refusals and cancellations interleave, every
// caller that took the slot released it, at most one held it at a time, and the
// gate is forgotten at the end.
func TestAcquireJoined_GrantRacingCancelAndRefusal_LeaksNothing(t *testing.T) {
	r := New()
	const callers = 200
	var holding atomic.Int32
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%7)*50*time.Microsecond)
			defer cancel()
			admitted := always
			if i%5 == 0 {
				var flips atomic.Int32
				admitted = func() bool { return flips.Add(1) < 3 }
			}
			release, err := r.AcquireJoined(ctx, "tx-1", 0, admitted)
			if err != nil {
				if release != nil {
					t.Error("a refused or cancelled caller was handed a release")
				}
				return
			}
			if n := holding.Add(1); n != 1 {
				t.Errorf("%d callers hold the slot at once", n)
			}
			time.Sleep(20 * time.Microsecond)
			holding.Add(-1)
			release()
		}()
		if i%3 == 0 {
			go r.Reevaluate("tx-1")
		}
	}
	wg.Wait()

	// The slot is free: a fresh caller takes it at once.
	got := acquireAsync(r, context.Background(), "tx-1", 0, always)
	expectSoon(t, got, "the slot after the storm").release()
	waitFor(t, "the gate to be forgotten", func() bool { return r.len() == 0 })
}

// --- the cap -----------------------------------------------------------------

// A transaction's gate takes a bounded queue. Past the cap AcquireJoined refuses
// instead of parking another caller: a compute member that fires callbacks
// faster than they are served would otherwise hold a request's worth of memory
// per callback for the life of the callout.
func TestAcquireJoined_RefusesPastTheWaiterCap(t *testing.T) {
	r := New()
	holder := mustAcquire(t, r, "tx-1")

	queued := acquireAsync(r, context.Background(), "tx-1", 1, always)
	waitFor(t, "the first waiter to reach the gate", func() bool { return r.AtCapacity("tx-1", 1) })

	release, err := r.AcquireJoined(context.Background(), "tx-1", 1, always)
	if !errors.Is(err, ErrTooManyWaiters) {
		t.Fatalf("AcquireJoined err = %v; want ErrTooManyWaiters", err)
	}
	if release != nil {
		t.Fatal("a refused caller was handed a gate")
	}

	// The holder and the waiter already queued are untouched: the queue drains.
	holder()
	got := expectSoon(t, queued, "the queued waiter to get the gate")
	if got.err != nil {
		t.Fatalf("the one waiter a cap of 1 admits was refused: %v", got.err)
	}
	got.release()
	if n := r.len(); n != 0 {
		t.Errorf("gate map = %d entries; a refusal must leave nothing behind", n)
	}
}

// The count is of callers waiting, not of the holder: a cap of n admits the
// holder plus n queued.
func TestAcquireJoined_TheHolderIsNotAWaiter(t *testing.T) {
	r := New()
	release, err := r.AcquireJoined(context.Background(), "tx-1", 1, always)
	if err != nil {
		t.Fatalf("the first caller of a free gate was refused: %v", err)
	}
	defer release()
	if r.AtCapacity("tx-1", 1) {
		t.Error("a gate with a holder and nobody queued reports itself full")
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
	release := mustAcquire(t, r, "tx-1")
	defer release()
	if r.AtCapacity("tx-1", 1) {
		t.Error("a gate nobody is queued for reports itself full")
	}
}

// --- the pre-coordination entry points ---------------------------------------

// AcquireCtx is AcquireJoined admitting everyone: capped, given up when its
// context ends.
func TestRegistry_AcquireCtx_IsCappedAndCancellable(t *testing.T) {
	r := New()
	holder := r.Acquire("tx-1")
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := r.AcquireCtx(ctx, "tx-1", 1)
		gaveUp <- err
	}()
	waitFor(t, "the waiter to queue", func() bool { return r.AtCapacity("tx-1", 1) })
	if _, err := r.AcquireCtx(context.Background(), "tx-1", 1); !errors.Is(err, ErrTooManyWaiters) {
		t.Fatalf("err = %v; want ErrTooManyWaiters", err)
	}
	cancel()
	if err := expectSoon(t, gaveUp, "the waiter to give up"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	holder()
	if n := r.len(); n != 0 {
		t.Fatalf("gate map = %d entries", n)
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
	expectSoon(t, got, "an uncapped wait to get the gate")
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
	expectNotYet(t, descendantAcquired, "descendant acquired gate(T) while the caller still held it")

	resume := Suspend(ctx)

	// Now the descendant progresses (the gate was released).
	expectSoon(t, descendantAcquired, "descendant to acquire gate(T) after Suspend")

	// resume() must block until the descendant releases, then re-acquire.
	resume()
	select {
	case <-descendantReleased:
	default:
		t.Fatal("resume() returned before the descendant released — it did not re-acquire the gate")
	}

	// The caller's release variable now points at the re-acquired gate; calling
	// it must free the gate (a fresh Acquire must not block afterward).
	release()
	done := make(chan struct{})
	go func() { r.Acquire(txID)(); close(done) }()
	expectSoon(t, done, "the gate after the caller's release()")
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after all releases, got %d", n)
	}
}
