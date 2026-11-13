package modelcache_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
)

// stubStore counts Get calls and returns a preset descriptor.
type stubStore struct {
	mu   sync.Mutex
	gets int
	desc *spi.ModelDescriptor
	err  error
	// delay introduces latency on Get to help test singleflight.
	delay time.Duration
}

func (s *stubStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.desc, s.err
}
func (s *stubStore) Save(context.Context, *spi.ModelDescriptor) error     { return nil }
func (s *stubStore) GetAll(context.Context) ([]spi.ModelRef, error)       { return nil, nil }
func (s *stubStore) Delete(context.Context, spi.ModelRef) error           { return nil }
func (s *stubStore) Lock(context.Context, spi.ModelRef) error             { return nil }
func (s *stubStore) Unlock(context.Context, spi.ModelRef) error           { return nil }
func (s *stubStore) IsLocked(context.Context, spi.ModelRef) (bool, error) { return false, nil }
func (s *stubStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *stubStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

func (s *stubStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newModelStore builds the per-request model store a CachingStoreFactory hands
// out over inner — the production read and write path through the shared
// cache.
func newModelStore(t *testing.T, inner spi.ModelStore, bc spi.ClusterBroadcaster, clk modelcache.Clock, lease time.Duration) spi.ModelStore {
	t.Helper()
	f := modelcache.NewCachingStoreFactory(&stubFactory{store: inner}, bc, clk, lease)
	ms, err := f.ModelStore(context.Background())
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	return ms
}

func lockedDescriptor(ref spi.ModelRef, level spi.ChangeLevel) *spi.ModelDescriptor {
	return &spi.ModelDescriptor{
		Ref:         ref,
		State:       spi.ModelLocked,
		ChangeLevel: level,
		Schema:      []byte(`{"ok":true}`),
	}
}

func TestCache_AdmitsAnyLockedDescriptor(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, spi.ChangeLevelStructural)}
	clk := &manualClock{now: time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)}
	c := newModelStore(t, inner, nil, clk, time.Hour)

	_, _ = c.Get(context.Background(), ref)
	_, _ = c.Get(context.Background(), ref)
	if inner.getCount() != 1 {
		t.Errorf("expected 1 inner Get (second call cached), got %d", inner.getCount())
	}
}

func TestCache_DoesNotAdmitUnlocked(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	desc := &spi.ModelDescriptor{Ref: ref, State: spi.ModelUnlocked, Schema: []byte(`{}`)}
	inner := &stubStore{desc: desc}
	clk := &manualClock{now: time.Now()}
	c := newModelStore(t, inner, nil, clk, time.Hour)

	_, _ = c.Get(context.Background(), ref)
	_, _ = c.Get(context.Background(), ref)
	if inner.getCount() != 2 {
		t.Errorf("expected 2 inner Gets (unlocked bypasses cache), got %d", inner.getCount())
	}
}

func TestCache_TTLExpiryForcesReload(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, "")}
	clk := &manualClock{now: time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)}
	c := newModelStore(t, inner, nil, clk, time.Hour)

	_, _ = c.Get(context.Background(), ref)
	clk.advance(2 * time.Hour) // well past jittered band
	_, _ = c.Get(context.Background(), ref)
	if inner.getCount() != 2 {
		t.Errorf("expected 2 inner Gets after TTL expiry, got %d", inner.getCount())
	}
}

func TestCache_InvalidateOnSave(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, "")}
	clk := &manualClock{now: time.Now()}
	c := newModelStore(t, inner, nil, clk, time.Hour)
	_, _ = c.Get(context.Background(), ref)
	_ = c.Save(context.Background(), inner.desc)
	_, _ = c.Get(context.Background(), ref)
	if inner.getCount() != 2 {
		t.Errorf("Save did not invalidate cache: inner.gets=%d", inner.getCount())
	}
}

func TestCache_InvalidateOnExtendSchema(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, "")}
	clk := &manualClock{now: time.Now()}
	c := newModelStore(t, inner, nil, clk, time.Hour)

	_, _ = c.Get(context.Background(), ref)
	_ = c.ExtendSchema(context.Background(), ref, spi.SchemaDelta(`[]`))
	_, _ = c.Get(context.Background(), ref)
	if inner.getCount() != 2 {
		t.Errorf("ExtendSchema did not invalidate cache: inner.gets=%d", inner.getCount())
	}
}

// TestCachingModelStore_ExtendSchema_ReturnsPostExtensionSchema — B-I8.
// Stronger than TestCache_InvalidateOnExtendSchema (which only asserts the
// cache was bypassed via inner.getCount): this test verifies the
// user-observable invariant that the post-ExtendSchema Get returns the NEW
// schema bytes, not the stale cached ones.
func TestCachingModelStore_ExtendSchema_ReturnsPostExtensionSchema(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	v1 := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: []byte(`{"v":1}`),
	}
	v2 := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: []byte(`{"v":2,"extended":true}`),
	}
	inner := &stubStore{desc: v1}
	clk := &manualClock{now: time.Now()}
	c := newModelStore(t, inner, nil, clk, time.Hour)

	// Warm the cache with v1.
	desc1, err := c.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("warm Get: %v", err)
	}
	if got, want := string(desc1.Schema), string(v1.Schema); got != want {
		t.Fatalf("warm Get schema = %q, want %q", got, want)
	}

	// Simulate the inner store now holding the post-extension schema.
	inner.mu.Lock()
	inner.desc = v2
	inner.mu.Unlock()

	// ExtendSchema through the cache — invalidation must fire.
	if err := c.ExtendSchema(context.Background(), ref, spi.SchemaDelta(`[]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	// Next Get must see v2 (cache was invalidated → reload from inner).
	desc2, err := c.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("post-extend Get: %v", err)
	}
	if got, want := string(desc2.Schema), string(v2.Schema); got != want {
		t.Errorf("post-extend Get returned stale cached schema: got %q, want %q", got, want)
	}
}

func TestCache_RefreshAndGet_Singleflight(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{
		desc:  lockedDescriptor(ref, ""),
		delay: 50 * time.Millisecond,
	}
	clk := &manualClock{now: time.Now()}
	c, ok := newModelStore(t, inner, nil, clk, time.Hour).(interface {
		RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error)
	})
	if !ok {
		t.Fatal("model store has no RefreshAndGet")
	}

	// 10 concurrent RefreshAndGet calls — should coalesce into 1 inner.Get.
	var wg sync.WaitGroup
	var callErrs atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.RefreshAndGet(context.Background(), ref); err != nil {
				callErrs.Add(1)
			}
		}()
	}
	wg.Wait()

	if callErrs.Load() != 0 {
		t.Errorf("got %d errors from concurrent RefreshAndGet", callErrs.Load())
	}
	// singleflight collapses concurrent calls — 1 or 2 depending on scheduling,
	// but definitely not 10.
	if inner.getCount() > 2 {
		t.Errorf("singleflight failed to collapse: inner.gets=%d (expected ≤ 2)", inner.getCount())
	}
}
