package auth

import (
	"context"
	"errors"
	"fmt"
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

// stringDecode keeps every value as its string; a value "bad" fails, "skip" is skipped.
func stringDecode(kvKey string, data []byte) (string, string, bool, error) {
	switch string(data) {
	case "bad":
		return "", "", false, errors.New("bad record")
	case "skip":
		return "", "", false, nil
	}
	return kvKey, string(data), true, nil
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

func TestReplica_LoadSkipsAndFailsStrict(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	_ = kv.Put(ctx, "ns", "s", []byte("skip"))
	r := newStringReplica(t, kv, nil)
	if got := snapshot(r); got["a"] != "1" || len(got) != 1 || r.skippedAtLoad != 1 {
		t.Fatalf("copy = %v skipped = %d", got, r.skippedAtLoad)
	}
	_ = kv.Put(ctx, "ns", "b", []byte("bad"))
	if _, err := newKVReplica(ctx, kv, replicaConfig[string]{name: "test", namespace: "ns", decode: stringDecode}); err == nil {
		t.Fatal("initial load must fail on an undecodable record")
	}
}

func TestReplica_ReconcileSwapsAndSkipsBadOnReRead(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	r := newStringReplica(t, kv, nil)
	_ = kv.Put(ctx, "ns", "a", []byte("1"))
	_ = kv.Put(ctx, "ns", "b", []byte("bad"))
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

type listHookKV struct {
	spi.KeyValueStore
	onList func()
	gets   atomic.Int32
	onGet  func()
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

func (h *listHookKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	v, err := h.KeyValueStore.Get(ctx, ns, key)
	if h.gets.Add(1) == 1 && h.onGet != nil {
		h.onGet()
	}
	return v, err
}

// A single-record load that raced a delete must not put the record back.
func TestReplica_LoadOneIsGenerationGuarded(t *testing.T) {
	ctx := replicaSystemCtx()
	base := newReplicaKV(t)
	_ = base.Put(ctx, "ns", "k", []byte("v"))
	kv := &listHookKV{KeyValueStore: base}
	r := newStringReplica(t, kv, nil)
	r.read(func(m map[string]string) {}) // loaded
	_ = r.mutate(func() (func(map[string]string), bool, error) {
		return func(m map[string]string) { delete(m, "k") }, false, nil
	})
	kv.onGet = func() {
		_ = r.mutate(func() (func(map[string]string), bool, error) {
			_ = base.Delete(ctx, "ns", "k")
			return func(m map[string]string) { delete(m, "k") }, true, nil
		})
	}
	_, found, err := r.loadOne(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if found || snapshot(r)["k"] != "" {
		t.Fatal("a stale single-record read overwrote a newer delete")
	}
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
	r := newStringReplica(t, kv, nil)
	kv.fail.Store(true)
	// Longer than stalenessMultiplier(10) x the 50ms interval: a naive
	// "has the bound elapsed" check would already call this stale, even
	// though the reconcile loop was never started.
	time.Sleep(700 * time.Millisecond)
	if r.Stale() {
		t.Fatal("stale before the loop started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
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
