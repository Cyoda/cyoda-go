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

// A re-read that overlaps a local change is discarded and retried.
func TestReplica_ReconcileYieldsToLocalChange(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := &listHookKV{KeyValueStore: newReplicaKV(t)}
	r := newStringReplica(t, kv, nil)
	var once sync.Once
	kv.onList = func() {
		once.Do(func() {
			_ = r.mutate(func() (func(map[string]string), bool, error) {
				_ = kv.KeyValueStore.Put(ctx, "ns", "local", []byte("v"))
				return func(m map[string]string) { m["local"] = "v" }, false, nil
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

func (h *listHookKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	if h.onList != nil {
		h.onList()
	}
	return h.KeyValueStore.List(ctx, ns)
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
	if r.Stale() {
		t.Fatal("stale before the loop started")
	}
	kv.fail.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
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
