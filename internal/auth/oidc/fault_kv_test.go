package oidc

import (
	"context"
	"fmt"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// FaultInjectingKV wraps a spi.KeyValueStore and provides deterministic pause
// injection for concurrency tests. After a registered operation completes, the
// wrapper signals the test on a channel, then blocks until the test sends back,
// giving the test precise control over goroutine interleaving.
//
// All methods delegate transparently when no pause is registered.
type FaultInjectingKV struct {
	Inner spi.KeyValueStore

	mu         sync.Mutex
	pauseAfter map[string]chan struct{}
}

// NewFaultInjectingKV wraps inner. Callers register pause points via PauseAt
// before the goroutine that triggers the operation is started.
func NewFaultInjectingKV(inner spi.KeyValueStore) *FaultInjectingKV {
	return &FaultInjectingKV{
		Inner:      inner,
		pauseAfter: make(map[string]chan struct{}),
	}
}

// PauseAt registers a pause point identified by opName. The format is:
//
//	"Put:<namespace>:<key>"
//	"Get:<namespace>:<key>"
//	"Delete:<namespace>:<key>"
//
// When the corresponding operation completes, the wrapper sends on the returned
// channel (signalling the test "op is done") and then blocks reading from the
// same channel (waiting for the test to send "you may continue").
//
// The channel is UNBUFFERED so both handshakes are true rendezvous. A buffered
// channel is unsafe here: maybeWait sends then immediately receives on the same
// channel, so with a buffer the worker goroutine can consume its own signal back
// before the test receives it — leaving the test's receive blocked forever (an
// intermittent deadlock under randomized scheduling). An unbuffered send cannot
// complete without a distinct receiver, so the worker can never satisfy its own
// wait.
func (k *FaultInjectingKV) PauseAt(opName string) chan struct{} {
	ch := make(chan struct{})
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pauseAfter[opName] = ch
	return ch
}

// maybeWait checks whether a pause is registered for opKey and, if so, signals
// the test then blocks until the test releases the goroutine.
func (k *FaultInjectingKV) maybeWait(opKey string) {
	k.mu.Lock()
	ch, ok := k.pauseAfter[opKey]
	k.mu.Unlock()
	if !ok {
		return
	}
	// Signal: op completed.
	ch <- struct{}{}
	// Block: wait for test to release.
	<-ch
}

// Put delegates to Inner, then pauses if a pause is registered for
// "Put:<namespace>:<key>".
func (k *FaultInjectingKV) Put(ctx context.Context, namespace, key string, value []byte) error {
	err := k.Inner.Put(ctx, namespace, key, value)
	k.maybeWait(fmt.Sprintf("Put:%s:%s", namespace, key))
	return err
}

// Get delegates to Inner, then pauses if a pause is registered for
// "Get:<namespace>:<key>".
func (k *FaultInjectingKV) Get(ctx context.Context, namespace, key string) ([]byte, error) {
	val, err := k.Inner.Get(ctx, namespace, key)
	k.maybeWait(fmt.Sprintf("Get:%s:%s", namespace, key))
	return val, err
}

// Delete delegates to Inner, then pauses if a pause is registered for
// "Delete:<namespace>:<key>".
func (k *FaultInjectingKV) Delete(ctx context.Context, namespace, key string) error {
	err := k.Inner.Delete(ctx, namespace, key)
	k.maybeWait(fmt.Sprintf("Delete:%s:%s", namespace, key))
	return err
}

// List delegates to Inner without pause injection (List has no single-key
// identity to discriminate on).
func (k *FaultInjectingKV) List(ctx context.Context, namespace string) (map[string][]byte, error) {
	return k.Inner.List(ctx, namespace)
}

// Compile-time assertion that FaultInjectingKV implements spi.KeyValueStore.
var _ spi.KeyValueStore = (*FaultInjectingKV)(nil)

// ---------------------------------------------------------------------------
// Smoke test
// ---------------------------------------------------------------------------

func TestFaultInjectingKV_PauseAtPut(t *testing.T) {
	inner := newTestKV(t)
	f := NewFaultInjectingKV(inner)

	const ns = "oidc-providers"
	const key = "t1:p1"

	ch := f.PauseAt("Put:" + ns + ":" + key)

	done := make(chan error, 1)
	go func() {
		done <- f.Put(context.Background(), ns, key, []byte("v"))
	}()

	// Wait until Put has completed (Inner.Put returned).
	<-ch

	// At this point Inner.Put has finished — the value must be visible.
	val, err := f.Get(context.Background(), ns, key)
	if err != nil || string(val) != "v" {
		t.Errorf("Put didn't complete before pause: err=%v val=%q", err, val)
	}

	// Unblock the goroutine.
	ch <- struct{}{}

	if err := <-done; err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
}

func TestFaultInjectingKV_NoRegisteredPause_DelegatesTransparently(t *testing.T) {
	inner := newTestKV(t)
	f := NewFaultInjectingKV(inner)

	ctx := context.Background()
	const ns = "oidc-providers"

	if err := f.Put(ctx, ns, "key1", []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	val, err := f.Get(ctx, ns, "key1")
	if err != nil || string(val) != "hello" {
		t.Errorf("Get: err=%v val=%q", err, val)
	}

	entries, err := f.List(ctx, ns)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if string(entries["key1"]) != "hello" {
		t.Errorf("List missing key1: %v", entries)
	}

	if err := f.Delete(ctx, ns, "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = f.Get(ctx, ns, "key1")
	if err == nil {
		t.Error("expected error after Delete, got nil")
	}
}
