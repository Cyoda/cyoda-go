package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
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
