package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// hookKV wraps a real KeyValueStore with fault-injection and interception
// hooks for reconcile tests.
type hookKV struct {
	spi.KeyValueStore
	mu      sync.Mutex
	listErr error
	onList  func()            // runs before each delegated List
	overlay map[string][]byte // extra entries merged into List results
}

func (h *hookKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	h.mu.Lock()
	le, ol := h.listErr, h.onList
	h.mu.Unlock()
	if le != nil {
		return nil, le
	}
	if ol != nil {
		ol()
	}
	m, err := h.KeyValueStore.List(ctx, ns)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, v := range h.overlay {
		m[k] = v
	}
	return m, nil
}

func (h *hookKV) setListErr(err error) { h.mu.Lock(); defer h.mu.Unlock(); h.listErr = err }
func (h *hookKV) setOnList(f func())   { h.mu.Lock(); defer h.mu.Unlock(); h.onList = f }
func (h *hookKV) setOverlay(k string, v []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overlay == nil {
		h.overlay = map[string][]byte{}
	}
	h.overlay[k] = v
}

func newTrustedKey(t *testing.T, kid string, tenant spi.TenantID) *auth.TrustedKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	exp := time.Now().Add(24 * time.Hour)
	return &auth.TrustedKey{
		KID: kid, TenantID: tenant, PublicKey: &key.PublicKey,
		Audience: "api://test", Active: true,
		ValidFrom: time.Now().Add(-time.Hour), ValidTo: &exp,
	}
}

// twoStores returns two KVTrustedKeyStore instances over one shared KV —
// the two-nodes-one-database seam.
func twoStores(t *testing.T) (*auth.KVTrustedKeyStore, *auth.KVTrustedKeyStore, *hookKV, context.Context) {
	t.Helper()
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	hkv := &hookKV{KeyValueStore: kv}
	s1, err := auth.NewKVTrustedKeyStore(ctx, hkv)
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	s2, err := auth.NewKVTrustedKeyStore(ctx, hkv)
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}
	return s1, s2, hkv, ctx
}

// T1: cross-node convergence via Reconcile for every mutation kind.
func TestKVTrustedKeyStore_Reconcile_ConvergesAcrossNodes(t *testing.T) {
	s1, s2, _, ctx := twoStores(t)
	tk := newTrustedKey(t, "conv-key", spi.SystemTenantID)

	// Register on node 1 → invisible to node 2's enumeration → reconcile fixes.
	if err := s1.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := s2.ListForVerification(); len(got) != 0 {
		t.Fatalf("pre-reconcile: node 2 already sees %d keys", len(got))
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := s2.ListForVerification(); len(got) != 1 || got[0].KID != "conv-key" {
		t.Fatalf("post-reconcile: want [conv-key], got %v", got)
	}
	if got := s2.List(spi.SystemTenantID); len(got) != 1 {
		t.Fatalf("List after reconcile: want 1, got %d", len(got))
	}

	// Invalidate on node 1 → node 2 reconcile picks up Active=false.
	if err := s1.Invalidate(spi.SystemTenantID, "conv-key", 0); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// gracePeriod 0 ⇒ ValidTo=now ⇒ excluded from verification listing.
	if got := s2.ListForVerification(); len(got) != 0 {
		t.Fatalf("invalidated key still verifiable on node 2: %v", got)
	}

	// Delete on node 1 → gone from node 2 after reconcile (Get included).
	if err := s1.Delete(spi.SystemTenantID, "conv-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := s2.Get(spi.SystemTenantID, "conv-key"); err == nil {
		t.Fatal("deleted key still Get-able on node 2 after reconcile")
	}
}

// T2a: a corrupt record is skipped; the rest reconcile.
func TestKVTrustedKeyStore_Reconcile_SkipsCorruptRecord(t *testing.T) {
	s1, s2, hkv, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "good-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	hkv.setOverlay(string(spi.SystemTenantID)+":corrupt", []byte("{not json"))
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile with corrupt record must not fail: %v", err)
	}
	if got := s2.ListForVerification(); len(got) != 1 || got[0].KID != "good-key" {
		t.Fatalf("good key lost during lenient reconcile: %v", got)
	}
}

// T2b: transport failure aborts the tick and keeps last-known state.
func TestKVTrustedKeyStore_Reconcile_TransportFailureKeepsState(t *testing.T) {
	s1, _, hkv, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "keep-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s1.Reconcile(ctx); err != nil {
		t.Fatalf("baseline Reconcile: %v", err)
	}
	hkv.setListErr(errors.New("kv down"))
	if err := s1.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile must return the transport error")
	}
	if got := s1.ListForVerification(); len(got) != 1 {
		t.Fatalf("state lost on transport failure: %v", got)
	}
}

// T3: a mutation landing between List and swap is not clobbered.
func TestKVTrustedKeyStore_Reconcile_GenerationGuard(t *testing.T) {
	s1, s2, hkv, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "doomed", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("warm-up Reconcile: %v", err)
	}
	// First List of the next reconcile sees "doomed"; the hook then deletes it
	// THROUGH s2 (bumping s2's generation) so the stale snapshot must be
	// discarded and rebuilt.
	fired := false
	hkv.setOnList(func() {
		if fired {
			return
		}
		fired = true
		if err := s2.Delete(spi.SystemTenantID, "doomed"); err != nil {
			t.Errorf("hook Delete: %v", err)
		}
	})
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := s2.Get(spi.SystemTenantID, "doomed"); err == nil {
		t.Fatal("generation guard failed: deleted key resurrected by stale snapshot")
	}
}

// T4: overlapping Reconcile calls are serialized; final state matches KV.
func TestKVTrustedKeyStore_Reconcile_ConcurrentReconcilesSerialized(t *testing.T) {
	s1, s2, _, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "racer", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s2.Reconcile(ctx)
		}()
	}
	wg.Wait()
	if got := s2.ListForVerification(); len(got) != 1 {
		t.Fatalf("concurrent reconciles corrupted state: %v", got)
	}
}

// fakeBroadcaster delivers published messages synchronously to every
// subscriber, including the publisher's own node (mirrors gossip loopback
// being absent — so tests subscribe a SECOND store to observe propagation).
type fakeBroadcaster struct {
	mu       sync.Mutex
	handlers map[string][]func([]byte)
}

func newFakeBroadcaster() *fakeBroadcaster {
	return &fakeBroadcaster{handlers: map[string][]func([]byte){}}
}

func (b *fakeBroadcaster) Broadcast(topic string, payload []byte) {
	b.mu.Lock()
	hs := append([]func([]byte){}, b.handlers[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		h(payload)
	}
}

func (b *fakeBroadcaster) Subscribe(topic string, h func([]byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = append(b.handlers[topic], h)
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// T5: a mutation on node 1 pings node 2, which reconciles.
func TestKVTrustedKeyStore_PingTriggersReconcile(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	b := newFakeBroadcaster()
	s1, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithTrustedKeyBroadcaster(b))
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	s2, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithTrustedKeyBroadcaster(b))
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}

	if err := s1.Register(newTrustedKey(t, "ping-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		return len(s2.ListForVerification()) == 1
	}, "node 2 never saw the registered key after ping")

	if err := s1.Delete(spi.SystemTenantID, "ping-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		return len(s2.ListForVerification()) == 0
	}, "node 2 never dropped the deleted key after ping")
}

// T7: the loop converges a peer mutation without any ping, respects the
// once-guard, and stops on ctx cancel.
func TestKVTrustedKeyStore_ReconcileLoop(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s1, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	s2, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithReconcileInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s2.StartReconcileLoop(loopCtx) {
		t.Fatal("first StartReconcileLoop returned false")
	}
	if s2.StartReconcileLoop(loopCtx) {
		t.Fatal("second StartReconcileLoop must be a no-op returning false")
	}

	if err := s1.Register(newTrustedKey(t, "loop-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		return len(s2.ListForVerification()) == 1
	}, "loop never converged the peer registration")

	// Cancel, then mutate again: node 2 must NOT converge (loop stopped).
	cancel()
	time.Sleep(100 * time.Millisecond) // let the loop goroutine observe cancel
	if err := s1.Delete(spi.SystemTenantID, "loop-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	// Use List (unaffected by the staleness breaker introduced later) so this
	// assertion stays a clean signal of "did reconcile happen" rather than
	// entangling it with the fail-closed staleness bound on ListForVerification.
	if got := s2.List(spi.SystemTenantID); len(got) != 1 {
		t.Fatalf("loop still reconciling after ctx cancel: %v", got)
	}
}

// T6: breaker fails the enumeration path closed, Get keeps serving KV
// ground truth, recovery restores, and the breaker is inert without a loop.
func TestKVTrustedKeyStore_StalenessBreaker(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	hkv := &hookKV{KeyValueStore: kv}
	s, err := auth.NewKVTrustedKeyStore(ctx, hkv, auth.WithReconcileInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.Register(newTrustedKey(t, "breaker-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s.StartReconcileLoop(loopCtx) {
		t.Fatal("StartReconcileLoop")
	}

	// Healthy loop: key verifiable.
	eventually(t, 2*time.Second, func() bool {
		return len(s.ListForVerification()) == 1
	}, "key not verifiable under healthy loop")

	// Kill List. Bound = 10×20ms = 200ms; wait comfortably past it.
	hkv.setListErr(errors.New("kv outage"))
	eventually(t, 5*time.Second, func() bool {
		return len(s.ListForVerification()) == 0
	}, "breaker never tripped: enumeration still serves stale keys")

	// Get during the trip: forced read-through still serves KV truth
	// (kv.Get is healthy — only List fails).
	if _, err := s.Get(spi.SystemTenantID, "breaker-key"); err != nil {
		t.Fatalf("Get during breaker trip must serve KV ground truth: %v", err)
	}

	// Recovery: List heals → breaker resets.
	hkv.setListErr(nil)
	eventually(t, 5*time.Second, func() bool {
		return len(s.ListForVerification()) == 1
	}, "breaker never recovered after KV healed")
}

func TestKVTrustedKeyStore_BreakerInertWithoutLoop(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithReconcileInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.Register(newTrustedKey(t, "no-loop-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // ≫ 10× interval, but no loop running
	if got := s.ListForVerification(); len(got) != 1 {
		t.Fatalf("breaker tripped without a reconcile loop: %v", got)
	}
}

// Finding 4 (coverage gap): no test previously drove Reconcile's retry
// budget to exhaustion. Mutating s2 itself on every List call (via the
// hookKV.onList seam) bumps s2's generation counter on every attempt, so
// the swap's generation guard never sees a stable snapshot.
func TestKVTrustedKeyStore_Reconcile_RetryBudgetExhausted(t *testing.T) {
	s1, s2, hkv, ctx := twoStores(t)

	// Already known to s2 before contention — must survive the failed
	// reconcile untouched (proves the breaker did not trip / cache wasn't
	// wiped).
	if err := s2.Register(newTrustedKey(t, "already-known", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register already-known: %v", err)
	}
	// Registered only on the peer (s1) — only a successful swap could ever
	// surface it on s2. Used to prove "no swap occurred".
	if err := s1.Register(newTrustedKey(t, "peer-only", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register peer-only: %v", err)
	}

	// Mutate s2 on EVERY List call so the generation guard never sees a
	// stable snapshot: Reconcile exhausts its retry budget.
	n := 0
	hkv.setOnList(func() {
		n++
		churn := newTrustedKey(t, fmt.Sprintf("churn-%d", n), spi.SystemTenantID)
		if err := s2.Register(churn, auth.RotateOptions{}); err != nil {
			t.Errorf("hook Register: %v", err)
		}
	})

	if err := s2.Reconcile(ctx); err == nil {
		t.Fatal("want an error when the retry budget is exhausted under continuous contention")
	}
	hkv.setOnList(nil)

	// No swap occurred: the peer-only key never reached s2's cache. Checked
	// via List (pure cache read) rather than Get, which deliberately
	// read-throughs to KV on a cache miss for multi-node visibility and
	// would therefore find peer-only regardless of whether the swap ran.
	if hasKID(s2.List(spi.SystemTenantID), "peer-only") {
		t.Fatal("contention exhaustion must not swap in a partial snapshot")
	}

	// consecutiveFailures accounting untouched — the already-known key
	// still serves (the staleness breaker never tripped from this
	// contention, which is not a KV failure).
	if got := s2.ListForVerification(); !hasKID(got, "already-known") {
		t.Fatalf("breaker must not trip on contention exhaustion; already-known key missing: %v", got)
	}

	// A subsequent clean Reconcile (no racing mutation) succeeds and
	// converges the peer-only key.
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("clean Reconcile after contention: %v", err)
	}
	if !hasKID(s2.List(spi.SystemTenantID), "peer-only") {
		t.Fatal("clean Reconcile after contention must converge peer-only key")
	}
}

func hasKID(keys []*auth.TrustedKey, kid string) bool {
	for _, tk := range keys {
		if tk.KID == kid {
			return true
		}
	}
	return false
}

// T5: a garbage payload is ignored (never parsed, never logged) and still
// triggers a harmless reconcile; the handler never panics.
func TestKVTrustedKeyStore_PingIgnoresPayload(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	b := newFakeBroadcaster()
	s, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithTrustedKeyBroadcaster(b))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	b.Broadcast("auth.trustedkeys", []byte("\x00garbage\xff of arbitrary peer bytes"))
	// Nothing to assert beyond absence of panic and continued liveness:
	if got := s.ListForVerification(); len(got) != 0 {
		t.Fatalf("unexpected keys: %v", got)
	}
}
