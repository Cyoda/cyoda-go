package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func replicaSystemCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "system", Tenant: spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
	})
}

func newReplicaKV(t *testing.T) spi.KeyValueStore {
	t.Helper()
	kv, err := memory.NewStoreFactory().KeyValueStore(replicaSystemCtx())
	if err != nil {
		t.Fatal(err)
	}
	return kv
}

// stringDecode keeps every value as its string.
func stringDecode(kvKey string, data []byte) (string, string) {
	return kvKey, string(data)
}

func newStringReplica(t *testing.T, kv spi.KeyValueStore, bc spi.ClusterBroadcaster) *kvReplica[string] {
	t.Helper()
	r, err := newKVReplica(replicaSystemCtx(), kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond, broadcaster: bc,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func snapshot(r *kvReplica[string]) map[string]string {
	out := map[string]string{}
	r.read(func(m map[string]string) {
		for k, v := range m {
			out[k] = v
		}
	})
	return out
}

func TestReplica_ReconcileSwaps(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	_ = kv.Put(ctx, "ns", "old", []byte("0"))
	r := newStringReplica(t, kv, nil)
	_ = kv.Delete(ctx, "ns", "old")
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(r); got["a"] != "1" || len(got) != 1 {
		t.Fatalf("copy = %v", got)
	}
}

type blockingKV struct {
	spi.KeyValueStore
	putEntered chan struct{}
	release    chan struct{}
	failKey    string
}

func (b *blockingKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if key == b.failKey {
		return fmt.Errorf("injected failure")
	}
	if b.putEntered != nil {
		b.putEntered <- struct{}{}
		<-b.release
	}
	return b.KeyValueStore.Put(ctx, ns, key, v)
}

// A slow store write under the admin mutex never blocks readers of the copy.
func TestReplica_MutateDoesNotBlockReaders(t *testing.T) {
	kv := &blockingKV{KeyValueStore: newReplicaKV(t), putEntered: make(chan struct{}), release: make(chan struct{})}
	r := newStringReplica(t, kv, nil)
	done := make(chan error)
	go func() {
		done <- r.mutate(func() (func(map[string]string), bool, error) {
			if err := r.writeAll(replicaSystemCtx(), []kvWrite{{key: "a", value: []byte("1")}}); err != nil {
				return nil, true, err
			}
			return func(m map[string]string) { m["a"] = "1" }, true, nil
		})
	}()
	<-kv.putEntered
	readDone := make(chan struct{})
	go func() { snapshot(r); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("a reader waited for a store write")
	}
	close(kv.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if snapshot(r)["a"] != "1" {
		t.Fatal("apply did not reach the copy")
	}
}

func TestReplica_WriteAllRestoresOnFailure(t *testing.T) {
	ctx := replicaSystemCtx()
	base := newReplicaKV(t)
	_ = base.Put(ctx, "ns", "old", []byte("before"))
	kv := &blockingKV{KeyValueStore: base, failKey: "third"}
	r := newStringReplica(t, kv, nil)
	err := r.writeAll(ctx, []kvWrite{
		{key: "new", value: []byte("n")},
		{key: "old", value: []byte("after"), prev: []byte("before")},
		{key: "third", value: []byte("x")},
	})
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	if _, err := base.Get(ctx, "ns", "new"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("new key not removed: %v", err)
	}
	if v, _ := base.Get(ctx, "ns", "old"); string(v) != "before" {
		t.Fatalf("old key = %q, want restored", v)
	}
}

// A fake Put that commits the write and then reports failure (e.g. a timeout
// after the store actually applied it).
type applyThenFailKV struct {
	spi.KeyValueStore
	failKey string
}

func (a *applyThenFailKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := a.KeyValueStore.Put(ctx, ns, key, v); err != nil {
		return err
	}
	if key == a.failKey {
		return fmt.Errorf("injected failure after commit")
	}
	return nil
}

// A Put/Delete that fails a write purely because ctx is already done — used
// to prove writeAll's restore pass runs on a context the caller cannot
// cancel, not the caller's own (possibly already-cancelled) context.
type ctxAwareKV struct {
	spi.KeyValueStore
	failKey string
	cancel  func()
}

func (c *ctxAwareKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == c.failKey {
		c.cancel()
		return fmt.Errorf("injected failure")
	}
	return c.KeyValueStore.Put(ctx, ns, key, v)
}

func (c *ctxAwareKV) Delete(ctx context.Context, ns, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.KeyValueStore.Delete(ctx, ns, key)
}

// A write that fails after already committing must still be restored itself,
// not just the writes before it: the store, not the return value, is ground
// truth for what landed.
func TestReplica_WriteAllRestoresTheFailedWriteItself(t *testing.T) {
	ctx := replicaSystemCtx()
	base := newReplicaKV(t)
	_ = base.Put(ctx, "ns", "old", []byte("before"))
	kv := &applyThenFailKV{KeyValueStore: base, failKey: "second"}
	r := newStringReplica(t, kv, nil)
	err := r.writeAll(ctx, []kvWrite{
		{key: "old", value: []byte("after"), prev: []byte("before")},
		{key: "second", value: []byte("x")},
	})
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	if v, _ := base.Get(ctx, "ns", "old"); string(v) != "before" {
		t.Fatalf("old key = %q, want restored", v)
	}
	if _, err := base.Get(ctx, "ns", "second"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("second key not removed even though its own Put committed before the injected failure: %v", err)
	}
}

// A caller whose context is cancelled exactly when the failing write happens
// (e.g. a request deadline) must still see every prior write restored: the
// restore pass runs on context.WithoutCancel, not the caller's context.
func TestReplica_WriteAllRestoresDespiteCallerCancellation(t *testing.T) {
	assertCtx := replicaSystemCtx()
	base := newReplicaKV(t)
	_ = base.Put(assertCtx, "ns", "old", []byte("before"))
	writeCtx, cancel := context.WithCancel(assertCtx)
	kv := &ctxAwareKV{KeyValueStore: base, failKey: "third", cancel: cancel}
	r := newStringReplica(t, kv, nil)
	err := r.writeAll(writeCtx, []kvWrite{
		{key: "old", value: []byte("after"), prev: []byte("before")},
		{key: "third", value: []byte("x")},
	})
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	if writeCtx.Err() == nil {
		t.Fatal("test setup bug: writeCtx should be cancelled by the failing write")
	}
	if v, _ := base.Get(assertCtx, "ns", "old"); string(v) != "before" {
		t.Fatalf("old key = %q, want restored despite the caller context being cancelled", v)
	}
}

// The originating node never receives its own gossip broadcast back. When an
// admin change writes the store but cannot describe a direct patch to the
// copy (a compensation, or another ambiguous partial write), the node must
// still converge to what is actually stored, on its own, without a caller
// explicitly re-running Reconcile.
func TestReplica_MutateWithoutApplyRefreshesOwnCopy(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	r := newStringReplica(t, kv, nil)
	err := r.mutate(func() (func(map[string]string), bool, error) {
		_ = kv.Put(ctx, "ns", "x", []byte("1"))
		return nil, true, errors.New("ambiguous write")
	})
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	deadline := time.Now().Add(2 * time.Second)
	for snapshot(r)["x"] != "1" {
		if time.Now().After(deadline) {
			t.Fatal("origin node never re-read its own write")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A peer change that this node re-reads while its own admin change is between
// the store write and the copy apply must not stay overwritten by that apply:
// the node converges back to the store on its own.
func TestReplica_MutateOverlappingReReadConverges(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	r := newStringReplica(t, kv, nil)
	err := r.mutate(func() (func(map[string]string), bool, error) {
		if err := kv.Put(ctx, "ns", "k", []byte("A")); err != nil {
			return nil, false, err
		}
		// A peer deletes k, and this node re-reads the store on its gossip.
		if err := kv.Delete(ctx, "ns", "k"); err != nil {
			return nil, true, err
		}
		reread := make(chan error)
		go func() { reread <- r.Reconcile(ctx) }()
		if err := <-reread; err != nil {
			return nil, true, err
		}
		if _, ok := snapshot(r)["k"]; ok {
			return nil, true, errors.New("re-read did not remove k")
		}
		return func(m map[string]string) { m["k"] = "A" }, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := snapshot(r)["k"]; !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("copy kept the overwritten value; it never converged to the store")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A re-read that overlaps a local change is discarded and retried. The hook
// fires only after the underlying List has already returned its (stale)
// snapshot, so the first attempt's fresh copy genuinely lacks "local" — the
// generation guard is the only thing that stops that stale snapshot from
// overwriting the change the mutate just committed.
func TestReplica_ReconcileYieldsToLocalChange(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := &listHookKV{KeyValueStore: newReplicaKV(t)}
	r := newStringReplica(t, kv, nil)
	var once sync.Once
	kv.onList = func() {
		once.Do(func() {
			_ = r.mutate(func() (func(map[string]string), bool, error) {
				if err := kv.KeyValueStore.Put(ctx, "ns", "local", []byte("v")); err != nil {
					return nil, false, err
				}
				return func(m map[string]string) { m["local"] = "v" }, true, nil
			})
		})
	}
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot(r)["local"] != "v" {
		t.Fatal("re-read overwrote a local change")
	}
}

// fakeReconcileMetrics counts calls instead of recording OTel gauges, so a
// test can assert whether a failure was counted at all.
type fakeReconcileMetrics struct {
	failureBumps atomic.Int32
	lastFailures atomic.Int32
}

func (f *fakeReconcileMetrics) SetReconcileConsecutiveFailures(n int) {
	f.failureBumps.Add(1)
	f.lastFailures.Store(int32(n))
}
func (f *fakeReconcileMetrics) SetReconcileStalenessSeconds(float64) {}

// TestReplica_ReconcileSuppressesLogAndMetricWhenCtxEnds covers the minor
// fix mirroring reapExpiredSnapshotsTick's own `if ctx.Err() != nil {
// return }` — scoped to r.ctx specifically, not whatever ctx this call
// happened to receive: a reconcile that fails because the REPLICA's own
// lifetime ended (the owner tearing it down, racing an in-flight List) is
// not a store failure, so it must not log a "reconcile failed" line or bump
// the consecutive-failure metric. The ctx passed to Reconcile here is the
// same object as r.ctx, so cancelling it is cancelling the replica's own
// lifetime — not just this one call's. A reconcile that fails for a genuine
// store reason while r.ctx is still alive still must log and bump —
// checked first, in the same test, so a change that silences logging
// unconditionally would also be caught. (The case where only this call's
// own ctx ends — e.g. a hung store's deadline — while r.ctx stays alive is
// covered separately, by TestReplica_ReconcileLogsAndBumpsMetricOnHungStoreWhileOwnerCtxAlive;
// suppressing that case too was the bug this split exists to avoid.)
func TestReplica_ReconcileSuppressesLogAndMetricWhenCtxEnds(t *testing.T) {
	kv := &failingListKV{KeyValueStore: newReplicaKV(t)}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeReconcileMetrics{}
	r.cfg.metrics = fm
	kv.fail.Store(true)

	var buf lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	// First, while r.ctx (== ctx) is still alive: a genuine store failure
	// must still log and bump the metric.
	if err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("want an error from the failing store")
	}
	logged := buf.String()
	if logged == "" {
		t.Fatal("a genuine store failure while r.ctx is alive logged nothing")
	}
	if n := fm.failureBumps.Load(); n != 1 {
		t.Fatalf("a genuine store failure bumped the failure metric %d times, want 1", n)
	}

	// Now end r.ctx itself — the replica's own lifetime, not just this next
	// call's: the same failing store must no longer log or bump.
	cancel()
	if err := r.Reconcile(ctx); err == nil {
		t.Fatal("want an error from the failing store")
	}
	if got := buf.String(); got != logged {
		t.Fatalf("a reconcile with r.ctx already ended logged another failure line: %s", got)
	}
	if n := fm.failureBumps.Load(); n != 1 {
		t.Fatalf("a reconcile with r.ctx already ended bumped the failure metric again: got %d, want still 1", n)
	}
}

// hangingListKV simulates a store call that hangs until its own ctx ends (a
// partition, an exhausted pool) and fails with that ctx's own error — never
// with an error of its own. Used to prove Reconcile distinguishes "this
// call's ctx ended because the store hung" from "the replica's owner ended
// its lifetime (r.ctx)": only the latter is suppressed from logging and
// metrics. Construction's own List call must go through unaffected (it has
// nothing to hang against yet), so hang starts false and the test flips it
// once construction returns.
type hangingListKV struct {
	spi.KeyValueStore
	hang atomic.Bool
}

func (h *hangingListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if !h.hang.Load() {
		return h.KeyValueStore.List(ctx, ns)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestReplica_ReconcileLogsAndBumpsMetricOnHungStoreWhileOwnerCtxAlive is
// the RED-first regression for the bug the reviewer's mutation probe found:
// reconcileOnce derives its per-call ctx via context.WithTimeout(r.ctx,
// interval), so a store that merely hangs until that per-call deadline — a
// partition, an exhausted connection pool — made List fail with
// DeadlineExceeded while r.ctx (the replica's own lifetime) was still very
// much alive. A fix that checks that per-call ctx's Err() instead of
// r.ctx's cannot tell those two apart, and silently drops the failure
// count, the staleness gauge and the log line for a real, ongoing store
// outage — exactly the kind of failure an operator most needs to see.
func TestReplica_ReconcileLogsAndBumpsMetricOnHungStoreWhileOwnerCtxAlive(t *testing.T) {
	kv := &hangingListKV{KeyValueStore: newReplicaKV(t)}
	r, err := newKVReplica(replicaSystemCtx(), kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 20 * time.Millisecond, // the per-call deadline reconcileOnce derives
	})
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeReconcileMetrics{}
	r.cfg.metrics = fm
	kv.hang.Store(true) // r.ctx (replicaSystemCtx()) stays alive for the whole test

	var buf lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	r.reconcileOnce() // the per-call ctx (WithTimeout(r.ctx, 20ms)) times out;
	// r.ctx itself never does.

	if buf.String() == "" {
		t.Fatal("a hung store (per-call deadline exceeded, owner ctx alive) logged nothing")
	}
	if n := fm.failureBumps.Load(); n != 1 {
		t.Fatalf("a hung store bumped the failure metric %d times, want 1", n)
	}
}

type listHookKV struct {
	spi.KeyValueStore
	onList func()
}

// List fetches first and runs the hook after, so a hook that mutates the
// store lands strictly after this call's own snapshot was taken — the
// snapshot returned to the caller is the stale one, same as a real race
// between an in-flight List and a concurrent local write.
func (h *listHookKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	v, err := h.KeyValueStore.List(ctx, ns)
	if h.onList != nil {
		h.onList()
	}
	return v, err
}

func TestReplica_PingTriggersReRead(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	bc := newInternalBroadcaster()
	a := newStringReplica(t, kv, bc)
	b := newStringReplica(t, kv, bc)
	_ = a.mutate(func() (func(map[string]string), bool, error) {
		_ = kv.Put(ctx, "ns", "x", []byte("1"))
		return func(m map[string]string) { m["x"] = "1" }, true, nil
	})
	deadline := time.Now().Add(2 * time.Second)
	for snapshot(b)["x"] != "1" {
		if time.Now().After(deadline) {
			t.Fatal("peer never re-read after the change message")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReplica_StaleOnlyAfterLoopStartsAndBound(t *testing.T) {
	kv := &failingListKV{KeyValueStore: newReplicaKV(t)}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		r.Wait()
	}()
	kv.fail.Store(true)
	// Longer than stalenessMultiplier(10) x the 50ms interval: a naive
	// "has the bound elapsed" check would already call this stale, even
	// though the reconcile loop was never started.
	time.Sleep(700 * time.Millisecond)
	if r.Stale() {
		t.Fatal("stale before the loop started")
	}
	if !r.Start() {
		t.Fatal("Start returned false on first call")
	}
	if r.Stale() {
		t.Fatal("stale immediately after Start: the gap before Start counted")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !r.Stale() {
		if time.Now().After(deadline) {
			t.Fatal("never became stale with failing re-reads")
		}
		time.Sleep(20 * time.Millisecond)
	}
	kv.fail.Store(false)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Stale() {
		t.Fatal("still stale after a successful re-read")
	}
}

// TestReplica_WaitBlocksUntilLoopExits proves the production API App.Close
// needs: cancelling r.ctx eventually stops the goroutine, and Wait blocks
// until it has, so a caller that cancels then Waits can rely on no further
// reconcile tick running afterward — including against a store that is
// itself being torn down concurrently with the cancel.
func TestReplica_WaitBlocksUntilLoopExits(t *testing.T) {
	kv := &listHookKV{KeyValueStore: newReplicaKV(t)}
	var listCount atomic.Int64
	kv.onList = func() { listCount.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err) // construction's own List call counts as 1
	}
	if !r.Start() {
		t.Fatal("Start returned false on first call")
	}

	// Wait for at least one periodic tick, so the loop is demonstrably
	// running before it is stopped.
	deadline := time.Now().Add(2 * time.Second)
	for listCount.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("periodic reconcile never ticked")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	r.Wait() // must not return before the loop goroutine has exited

	after := listCount.Load()
	time.Sleep(300 * time.Millisecond) // several intervals' worth of margin
	if got := listCount.Load(); got != after {
		t.Fatalf("a reconcile tick ran after Wait returned: at-wait=%d now=%d", after, got)
	}
}

// TestReplica_WaitReturnsImmediatelyWithoutStart covers the case where a
// component is constructed but Start is never called (e.g. a construction
// failure elsewhere aborts startup before Start runs): Wait must not block
// forever.
func TestReplica_WaitReturnsImmediatelyWithoutStart(t *testing.T) {
	kv := newReplicaKV(t)
	r := newStringReplica(t, kv, nil)
	done := make(chan struct{})
	go func() {
		r.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Wait blocked forever when Start was never called")
	}
}

// TestReplica_PingAfterCancelAndWaitCausesNoStoreRead proves the other half
// of the production shutdown contract Wait exists for: after r.ctx is
// cancelled and Wait returns, a gossip ping that arrives afterward (the
// broadcaster subscription set up at construction is never torn down) must
// not read the store at all — not even once, and no failure log for one.
// reconcileOnce checks r.ctx directly, so a ping-triggered reconcile sees it
// already done regardless of whether the periodic loop or the ping
// triggered it.
func TestReplica_PingAfterCancelAndWaitCausesNoStoreRead(t *testing.T) {
	kv := &listHookKV{KeyValueStore: newReplicaKV(t)}
	var listCount atomic.Int64
	kv.onList = func() { listCount.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Start() {
		t.Fatal("Start returned false on first call")
	}

	before := listCount.Load() // construction's own List call

	cancel()
	r.Wait()

	// Simulate a gossip ping arriving after teardown: the subscription is
	// never unsubscribed, so handlePing can still fire.
	r.handlePing(nil)
	r.ping.Wait() // block until the (expected no-op) triggered reconcile finishes

	if got := listCount.Load(); got != before {
		t.Fatalf("a gossip ping after cancel+Wait read the store: before=%d after=%d", before, got)
	}
}

// gatedListKV blocks the List call at and after skipFirst (construction's
// own List call, counted from 1, passes straight through unblocked) until
// release is closed, signalling entered the moment it is inside the gate.
// The read of waitReturned right after release fires, with its outcome
// recorded in failed and checked closed right after — never a direct
// t.Error/t.Fatal from this goroutine, which could otherwise race the test
// function's own return (the testing package panics if a *T is used after
// its test has returned, which is worse than just the race it would mask).
// The test itself asserts on failed only after checked confirms the read
// happened.
type gatedListKV struct {
	spi.KeyValueStore
	skipFirst    int
	calls        atomic.Int32
	entered      chan struct{}
	release      chan struct{}
	checked      chan struct{}
	waitReturned *atomic.Bool
	failed       atomic.Bool
}

func (k *gatedListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if int(k.calls.Add(1)) <= k.skipFirst {
		return k.KeyValueStore.List(ctx, ns)
	}
	close(k.entered)
	<-k.release
	k.failed.Store(k.waitReturned.Load())
	close(k.checked)
	return k.KeyValueStore.List(ctx, ns)
}

// TestReplica_WaitBlocksUntilInFlightTickFinishes is a gated-fake proof (not
// a count, which a no-op Wait can still pass by accident) that Wait does
// not return while the periodic loop's own List call is still in flight.
// Mutation-tested: making kvReplica.Wait a no-op turns this RED — cancel
// alone stops the loop eventually, but does not block the caller until the
// in-flight read actually finishes.
func TestReplica_WaitBlocksUntilInFlightTickFinishes(t *testing.T) {
	var waitReturned atomic.Bool
	kv := &gatedListKV{
		KeyValueStore: newReplicaKV(t), skipFirst: 1,
		entered: make(chan struct{}), release: make(chan struct{}), checked: make(chan struct{}),
		waitReturned: &waitReturned,
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Start() {
		t.Fatal("Start returned false on first call")
	}

	select {
	case <-kv.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the periodic tick never reached the store")
	}

	cancel()
	waitDone := make(chan struct{})
	go func() {
		r.Wait()
		waitReturned.Store(true)
		close(waitDone)
	}()
	// A no-op Wait returns essentially immediately; give it a generous
	// window to do so before releasing the gate, so the mutation reliably
	// shows red rather than racing it.
	time.Sleep(100 * time.Millisecond)

	close(kv.release)
	select {
	case <-kv.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("the gated List call never reached its check")
	}
	if kv.failed.Load() {
		t.Fatal("Wait returned while the in-flight tick's store read was still unresolved")
	}

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait never returned after the in-flight tick finished")
	}
}

// TestReplica_WaitBlocksUntilInFlightPingReconcileFinishes is the
// ping-triggered counterpart: Wait must not return while a reconcile that a
// gossip ping triggered is still in flight. Isolated from the periodic loop
// — Start is never called — so this exercises only kvReplica.Wait's call to
// r.ping.Wait(). Mutation-tested: removing that call turns this RED.
func TestReplica_WaitBlocksUntilInFlightPingReconcileFinishes(t *testing.T) {
	var waitReturned atomic.Bool
	kv := &gatedListKV{
		KeyValueStore: newReplicaKV(t), skipFirst: 1,
		entered: make(chan struct{}), release: make(chan struct{}), checked: make(chan struct{}),
		waitReturned: &waitReturned,
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	r.handlePing(nil) // triggers reconcileOnce on r.ping's own goroutine

	select {
	case <-kv.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the ping-triggered reconcile never reached the store")
	}

	cancel()
	waitDone := make(chan struct{})
	go func() {
		r.Wait()
		waitReturned.Store(true)
		close(waitDone)
	}()
	time.Sleep(100 * time.Millisecond) // see the sibling test for why

	close(kv.release)
	select {
	case <-kv.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("the gated List call never reached its check")
	}
	if kv.failed.Load() {
		t.Fatal("Wait returned while the in-flight ping reconcile's store read was still unresolved")
	}

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait never returned after the in-flight ping reconcile finished")
	}
}

type failingListKV struct {
	spi.KeyValueStore
	fail atomic.Bool
}

func (f *failingListKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if f.fail.Load() {
		return nil, errors.New("list down")
	}
	return f.KeyValueStore.List(ctx, ns)
}

type internalBroadcaster struct {
	mu sync.Mutex
	hs map[string][]func([]byte)
}

func newInternalBroadcaster() *internalBroadcaster {
	return &internalBroadcaster{hs: map[string][]func([]byte){}}
}

func (b *internalBroadcaster) Broadcast(topic string, p []byte) {
	b.mu.Lock()
	hs := append([]func([]byte){}, b.hs[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		h(p)
	}
}

func (b *internalBroadcaster) Subscribe(topic string, h func([]byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hs[topic] = append(b.hs[topic], h)
}

// immediateBroadcaster invokes the handler synchronously, inside Subscribe
// itself, then blocks for longer than a re-read normally takes. This
// simulates a gossip message that a real broadcaster could deliver — and
// have fully processed on the ping's own goroutine — before the constructor
// that is still setting up (newKVReplica itself, or a caller assigning its
// own reference to the result, such as KVKeyStore assigning s.rep only after
// newKVReplica returns) gets control back. Subscribe blocking is what makes
// the race deterministic enough to test: without it, the triggered re-read
// runs on a fresh goroutine that may or may not finish before the caller
// moves on.
type immediateBroadcaster struct{}

func (immediateBroadcaster) Broadcast(string, []byte) {}

func (immediateBroadcaster) Subscribe(_ string, h func([]byte)) {
	h(nil)
	time.Sleep(300 * time.Millisecond)
}

func newStringReplicaWithAfterChange(t *testing.T, kv spi.KeyValueStore, bc spi.ClusterBroadcaster, afterChange func(map[string]string)) *kvReplica[string] {
	t.Helper()
	r, err := newKVReplica(replicaSystemCtx(), kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond, broadcaster: bc, afterChange: afterChange,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// afterChange must receive the copy it applies (the swapped map on Reconcile,
// the applied map on mutate), not reach back into the replica for it: it runs
// after the copy lock is released, and the only safe way to hand it a
// consistent snapshot is to pass it directly under a read lock.
func TestReplica_AfterChangeReceivesRecsOnReconcile(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	var calls int
	var got map[string]string
	r := newStringReplicaWithAfterChange(t, kv, nil, func(recs map[string]string) {
		calls++
		got = map[string]string{}
		for k, v := range recs {
			got[k] = v
		}
	})
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got["a"] != "1" {
		t.Fatalf("calls = %d, got = %v", calls, got)
	}
}

func TestReplica_AfterChangeReceivesRecsOnMutate(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	var got map[string]string
	r := newStringReplicaWithAfterChange(t, kv, nil, func(recs map[string]string) {
		got = map[string]string{}
		for k, v := range recs {
			got[k] = v
		}
	})
	err := r.mutate(func() (func(map[string]string), bool, error) {
		_ = kv.Put(ctx, "ns", "x", []byte("1"))
		return func(m map[string]string) { m["x"] = "1" }, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["x"] != "1" {
		t.Fatalf("afterChange did not see mutate's result: %v", got)
	}
}

// A broadcaster that delivers a gossip message during Subscribe — before
// newKVReplica has even returned to its caller — must still only ever hand
// afterChange the recs argument, never require it to reach back into the
// replica (whose fields, or a caller's own reference to it such as
// KVKeyStore's s.rep, may not be fully assigned yet). immediateBroadcaster
// blocks Subscribe long enough that the ping's triggered reconcile has
// already run and called afterChange by the time construction returns, so
// this test requires the callback to have fired — it is not a maybe.
func TestReplica_AfterChangeDuringConstructionSeesOnlyRecs(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	called := make(chan map[string]string, 1)
	r, err := newKVReplica(ctx, kv, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond, broadcaster: immediateBroadcaster{},
		afterChange: func(recs map[string]string) {
			cp := map[string]string{}
			for k, v := range recs {
				cp[k] = v
			}
			called <- cp
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if got["a"] != "1" {
			t.Fatalf("afterChange saw = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("afterChange was never called for the construction-time gossip")
	}
	if snapshot(r)["a"] != "1" {
		t.Fatal("replica unusable after construction-time gossip")
	}
}

// replicaTxProbeKV records every call whose context carries a transaction:
// the postgres KV store would join it, so a signing-key or trusted-key write
// made under a caller's entity transaction (X-Tx-Token) would otherwise
// commit or roll back with that transaction instead of independently.
type replicaTxProbeKV struct {
	spi.KeyValueStore
	mu   sync.Mutex
	seen []string
}

func (k *replicaTxProbeKV) probe(ctx context.Context, op, ns string) {
	if spi.GetTransaction(ctx) == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seen = append(k.seen, op+" "+ns)
}

func (k *replicaTxProbeKV) calls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.seen...)
}

func (k *replicaTxProbeKV) Put(ctx context.Context, ns, key string, v []byte) error {
	k.probe(ctx, "Put", ns)
	return k.KeyValueStore.Put(ctx, ns, key, v)
}

func (k *replicaTxProbeKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	k.probe(ctx, "Get", ns)
	return k.KeyValueStore.Get(ctx, ns, key)
}

func (k *replicaTxProbeKV) Delete(ctx context.Context, ns, key string) error {
	k.probe(ctx, "Delete", ns)
	return k.KeyValueStore.Delete(ctx, ns, key)
}

func (k *replicaTxProbeKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	k.probe(ctx, "List", ns)
	return k.KeyValueStore.List(ctx, ns)
}

// TestKVReplica_IgnoresCallerTransaction proves that no KV call a kvReplica
// makes — construction's initial List, a re-read's List (Reconcile) or an
// admin write (writeAll/put) — carries a transaction found in the caller's
// context. That covers every call the replica itself makes; KVKeyStore also
// issues direct decision reads against the underlying kv (not through the
// replica), and those strip the transaction at method entry instead.
func TestKVReplica_IgnoresCallerTransaction(t *testing.T) {
	txCtx := spi.WithTransaction(replicaSystemCtx(), &spi.TransactionState{ID: "caller-tx"})

	probe := &replicaTxProbeKV{KeyValueStore: newReplicaKV(t)}
	r, err := newKVReplica(txCtx, probe, replicaConfig[string]{
		name: "test", namespace: "ns", topic: "t", decode: stringDecode,
		interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.writeAll(txCtx, []kvWrite{{key: "k1", value: []byte("v1")}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(txCtx); err != nil {
		t.Fatal(err)
	}
	// value: nil takes put's Delete branch — writeAll above never exercised
	// it, so removing noTx from that branch alone would not have failed
	// this test.
	if err := r.writeAll(txCtx, []kvWrite{{key: "k1", value: nil, prev: []byte("v1")}}); err != nil {
		t.Fatal(err)
	}

	if seen := probe.calls(); len(seen) != 0 {
		t.Fatalf("KV calls made under the caller's transaction: %v", seen)
	}
}
