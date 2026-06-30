package modelcache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
)

// stubFactory is a minimal spi.StoreFactory whose ModelStore returns
// the same stubStore reference for every call. The shared inner makes
// it easy to assert "N calls to factory.ModelStore(ctx) produced only
// M inner.Get calls" across independent adapters.
type stubFactory struct {
	store *stubStore
}

func (f *stubFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return f.store, nil
}
func (f *stubFactory) EntityStore(context.Context) (spi.EntityStore, error)       { return nil, nil }
func (f *stubFactory) KeyValueStore(context.Context) (spi.KeyValueStore, error)   { return nil, nil }
func (f *stubFactory) MessageStore(context.Context) (spi.MessageStore, error)     { return nil, nil }
func (f *stubFactory) WorkflowStore(context.Context) (spi.WorkflowStore, error)   { return nil, nil }
func (f *stubFactory) AsyncSearchStore(context.Context) (spi.AsyncSearchStore, error) {
	return nil, nil
}
func (f *stubFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	return nil, nil
}
func (f *stubFactory) TransactionManager(context.Context) (spi.TransactionManager, error) {
	return nil, nil
}
func (f *stubFactory) Close() error { return nil }

func TestCachingStoreFactory_SharesCacheAcrossRequests(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, spi.ChangeLevelStructural)}
	factory := modelcache.NewCachingStoreFactory(&stubFactory{store: inner}, nil, &manualClock{now: time.Now()}, time.Hour)

	ctx := withTenantContext(context.Background(), "t1")

	// Simulate three independent HTTP requests — each gets its own
	// ModelStore from the factory but they share one cache.
	for i := 0; i < 3; i++ {
		ms, err := factory.ModelStore(ctx)
		if err != nil {
			t.Fatalf("ModelStore #%d: %v", i, err)
		}
		if _, err := ms.Get(ctx, ref); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}

	if inner.getCount() != 1 {
		t.Errorf("expected 1 inner Get across 3 independent requests (cache hit on 2 & 3); got %d", inner.getCount())
	}
}

func TestCachingStoreFactory_InvalidateCrossRequest(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, "")}
	factory := modelcache.NewCachingStoreFactory(&stubFactory{store: inner}, nil, &manualClock{now: time.Now()}, time.Hour)
	ctx := withTenantContext(context.Background(), "t1")

	// Request 1: populate cache.
	ms1, _ := factory.ModelStore(ctx)
	_, _ = ms1.Get(ctx, ref)

	// Request 2: invalidate via Lock.
	ms2, _ := factory.ModelStore(ctx)
	_ = ms2.Lock(ctx, ref)

	// Request 3: miss, inner consulted again.
	ms3, _ := factory.ModelStore(ctx)
	_, _ = ms3.Get(ctx, ref)

	if inner.getCount() != 2 {
		t.Errorf("expected 2 inner Gets (before + after invalidation); got %d", inner.getCount())
	}
}

func TestCachingStoreFactory_TenantIsolation(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	inner := &stubStore{desc: lockedDescriptor(ref, "")}
	factory := modelcache.NewCachingStoreFactory(&stubFactory{store: inner}, nil, &manualClock{now: time.Now()}, time.Hour)

	ctxA := withTenantContext(context.Background(), "tenant-A")
	ctxB := withTenantContext(context.Background(), "tenant-B")

	msA, _ := factory.ModelStore(ctxA)
	_, _ = msA.Get(ctxA, ref)

	msB, _ := factory.ModelStore(ctxB)
	_, _ = msB.Get(ctxB, ref)

	if inner.getCount() != 2 {
		t.Errorf("cross-tenant same-ref must not share cache entries; got %d inner Gets", inner.getCount())
	}
}

func TestCachingStoreFactory_PassThroughOtherStores(t *testing.T) {
	// Factory's non-ModelStore methods must reach the inner factory.
	// Use a stubFactory that records which methods were called.
	rec := &recordingFactory{}
	factory := modelcache.NewCachingStoreFactory(rec, nil, &manualClock{now: time.Now()}, time.Hour)
	ctx := withTenantContext(context.Background(), "t1")

	_, _ = factory.EntityStore(ctx)
	_, _ = factory.KeyValueStore(ctx)
	_, _ = factory.MessageStore(ctx)
	_, _ = factory.WorkflowStore(ctx)
	_, _ = factory.AsyncSearchStore(ctx)
	_, _ = factory.StateMachineAuditStore(ctx)
	_, _ = factory.TransactionManager(ctx)

	for name, count := range rec.counts() {
		if count != 1 {
			t.Errorf("expected 1 call to inner %s, got %d", name, count)
		}
	}
}

type recordingFactory struct {
	mu sync.Mutex
	c  map[string]int
}

func (f *recordingFactory) bump(n string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.c == nil {
		f.c = map[string]int{}
	}
	f.c[n]++
}

func (f *recordingFactory) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.c))
	for k, v := range f.c {
		out[k] = v
	}
	return out
}

func (f *recordingFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	f.bump("EntityStore")
	return nil, nil
}
func (f *recordingFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	f.bump("ModelStore")
	return &stubStore{}, nil
}
func (f *recordingFactory) KeyValueStore(context.Context) (spi.KeyValueStore, error) {
	f.bump("KeyValueStore")
	return nil, nil
}
func (f *recordingFactory) MessageStore(context.Context) (spi.MessageStore, error) {
	f.bump("MessageStore")
	return nil, nil
}
func (f *recordingFactory) WorkflowStore(context.Context) (spi.WorkflowStore, error) {
	f.bump("WorkflowStore")
	return nil, nil
}
func (f *recordingFactory) AsyncSearchStore(context.Context) (spi.AsyncSearchStore, error) {
	f.bump("AsyncSearchStore")
	return nil, nil
}
func (f *recordingFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	f.bump("StateMachineAuditStore")
	return nil, nil
}
func (f *recordingFactory) TransactionManager(context.Context) (spi.TransactionManager, error) {
	f.bump("TransactionManager")
	return nil, nil
}
func (f *recordingFactory) Close() error { return nil }

// capableInnerFactory wraps stubFactory and implements spi.CompositeUniqueKeyCapable.
type capableInnerFactory struct {
	stubFactory
	supports bool
}

func (f *capableInnerFactory) SupportsCompositeUniqueKeys() bool { return f.supports }

// TestCachingStoreFactory_ForwardsCompositeUniqueKeyCapable verifies that
// the caching wrapper propagates the CompositeUniqueKeyCapable capability
// from the inner factory rather than silently hiding it.
func TestCachingStoreFactory_ForwardsCompositeUniqueKeyCapable(t *testing.T) {
	t.Run("inner capable and true", func(t *testing.T) {
		inner := &capableInnerFactory{supports: true}
		f := modelcache.NewCachingStoreFactory(inner, nil, &manualClock{now: time.Now()}, time.Hour)
		c, ok := any(f).(spi.CompositeUniqueKeyCapable)
		if !ok {
			t.Fatal("CachingStoreFactory must implement CompositeUniqueKeyCapable when inner does")
		}
		if !c.SupportsCompositeUniqueKeys() {
			t.Error("expected SupportsCompositeUniqueKeys=true when inner returns true")
		}
	})

	t.Run("inner capable and false", func(t *testing.T) {
		inner := &capableInnerFactory{supports: false}
		f := modelcache.NewCachingStoreFactory(inner, nil, &manualClock{now: time.Now()}, time.Hour)
		c, ok := any(f).(spi.CompositeUniqueKeyCapable)
		if !ok {
			t.Fatal("CachingStoreFactory must implement CompositeUniqueKeyCapable")
		}
		if c.SupportsCompositeUniqueKeys() {
			t.Error("expected SupportsCompositeUniqueKeys=false when inner returns false")
		}
	})

	t.Run("inner not capable", func(t *testing.T) {
		inner := &stubFactory{} // does not implement CompositeUniqueKeyCapable
		f := modelcache.NewCachingStoreFactory(inner, nil, &manualClock{now: time.Now()}, time.Hour)
		c, ok := any(f).(spi.CompositeUniqueKeyCapable)
		if !ok {
			t.Fatal("CachingStoreFactory must implement CompositeUniqueKeyCapable (returns false for non-capable inner)")
		}
		if c.SupportsCompositeUniqueKeys() {
			t.Error("expected SupportsCompositeUniqueKeys=false when inner does not implement the interface")
		}
	})
}
