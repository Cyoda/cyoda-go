package txgate

import (
	"context"
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
