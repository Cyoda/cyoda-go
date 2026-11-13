package modelcache

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func ctxFor(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u",
		Tenant: spi.Tenant{ID: spi.TenantID(tenant)},
	})
}

// recoverFrom runs f and returns what it panicked with, nil if it returned.
func recoverFrom(f func()) (panicked any) {
	defer func() { panicked = recover() }()
	f()
	return nil
}

type panicValue struct{ msg string }

// TestSharedRead_PanicReachesTheCaller: a panic in the storage read surfaces
// on the caller's goroutine, where the door's recovery answers it, never on
// the shared call's own goroutine, where nothing can recover it.
func TestSharedRead_PanicReachesTheCaller(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	want := &panicValue{msg: "read exploded"}
	inner.getFn = func(context.Context, int) error { panic(want) }
	s := &requestScopedStore{cache: c, inner: inner}

	got := recoverFrom(func() { _, _ = s.Get(tenantCtx(), testRef) })
	if got != want {
		t.Fatalf("caller recovered %v, want the read's panic value", got)
	}
	if c.testHasEntry(testKey()) {
		t.Error("a panicking read left an entry")
	}
}

// TestSharedExtend_PanicReachesTheCallerAndStillInvalidates: a panic in the
// storage extension surfaces on the caller's goroutine, and the shared call
// still evicts, bumps and broadcasts — the extension may have committed.
func TestSharedExtend_PanicReachesTheCallerAndStillInvalidates(t *testing.T) {
	bc := &recordingBroadcaster{}
	c := newTestCache(bc)
	inner := newFakeInner(lockedDesc())
	want := &panicValue{msg: "extension exploded"}
	inner.extFn = func(context.Context, int) error { panic(want) }
	s := &requestScopedStore{cache: c, inner: inner}

	if _, err := s.Get(tenantCtx(), testRef); err != nil {
		t.Fatalf("warm Get: %v", err)
	}
	genBefore := c.testGeneration(testKey())

	got := recoverFrom(func() { _ = s.ExtendSchema(tenantCtx(), testRef, testDelta) })
	if got != want {
		t.Fatalf("caller recovered %v, want the extension's panic value", got)
	}
	if c.testHasEntry(testKey()) {
		t.Error("entry still cached after a panicking extension call")
	}
	if g := c.testGeneration(testKey()); g != genBefore+1 {
		t.Errorf("generation = %d, want %d", g, genBefore+1)
	}
	if n := len(bc.sent()); n != 1 {
		t.Errorf("broadcasts = %d, want 1", n)
	}
}

// TestSharedRead_TenantIsolation: a read held for tenant A is never joined by
// tenant B's read of the same ref; B reads its own store.
func TestSharedRead_TenantIsolation(t *testing.T) {
	c := newTestCache(nil)
	innerA := newFakeInner(lockedDesc())
	gate := make(chan struct{})
	innerA.setGate(gate)
	descB := lockedDesc()
	innerB := newFakeInner(descB)
	sA := &requestScopedStore{cache: c, inner: innerA}
	sB := &requestScopedStore{cache: c, inner: innerB}

	doneA := make(chan error, 1)
	go func() {
		_, err := sA.Get(ctxFor("tenant-a"), testRef)
		doneA <- err
	}()
	defer func() {
		close(gate)
		if err := <-doneA; err != nil {
			t.Errorf("tenant A Get: %v", err)
		}
	}()
	waitSignal(t, innerA.getEntered, "tenant A's read to reach storage")
	waitParked(t, 1)

	type result struct {
		desc *spi.ModelDescriptor
		err  error
	}
	doneB := make(chan result, 1)
	go func() {
		d, err := sB.Get(ctxFor("tenant-b"), testRef)
		doneB <- result{d, err}
	}()
	select {
	case r := <-doneB:
		if r.err != nil {
			t.Fatalf("tenant B Get: %v", r.err)
		}
		if r.desc != descB {
			t.Error("tenant B was not answered from its own store")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tenant B's read is waiting on tenant A's")
	}
	if a, b := innerA.getCount(), innerB.getCount(); a != 1 || b != 1 {
		t.Errorf("inner Get calls A=%d B=%d, want 1 each", a, b)
	}
}

// TestSharedExtend_TenantIsolation: an extension held for tenant A is never
// joined by tenant B's identical extension of the same ref; B extends its own
// store.
func TestSharedExtend_TenantIsolation(t *testing.T) {
	c := newTestCache(nil)
	innerA := newFakeInner(lockedDesc())
	release := make(chan struct{})
	innerA.extFn = func(context.Context, int) error {
		<-release
		return nil
	}
	innerB := newFakeInner(lockedDesc())
	sA := &requestScopedStore{cache: c, inner: innerA}
	sB := &requestScopedStore{cache: c, inner: innerB}

	doneA := make(chan error, 1)
	go func() { doneA <- sA.ExtendSchema(ctxFor("tenant-a"), testRef, testDelta) }()
	defer func() {
		close(release)
		if err := <-doneA; err != nil {
			t.Errorf("tenant A ExtendSchema: %v", err)
		}
	}()
	waitSignal(t, innerA.extEntered, "tenant A's extension to reach storage")
	waitParked(t, 1)

	doneB := make(chan error, 1)
	go func() { doneB <- sB.ExtendSchema(ctxFor("tenant-b"), testRef, testDelta) }()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("tenant B ExtendSchema: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tenant B's extension is waiting on tenant A's")
	}
	a, _ := innerA.extCount()
	b, _ := innerB.extCount()
	if a != 1 || b != 1 {
		t.Errorf("inner ExtendSchema calls A=%d B=%d, want 1 each", a, b)
	}
}

// TestSchemaNode_DroppedFillIsParsedNotRetained: when the generation guard
// drops SchemaNode's fill, SchemaNode answers with the parse of the descriptor
// it read, and nothing is cached.
func TestSchemaNode_DroppedFillIsParsedNotRetained(t *testing.T) {
	root := schema.NewObjectNode()
	root.SetChild("amount", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	c := newTestCache(nil)
	inner := newFakeInner(&spi.ModelDescriptor{Ref: testRef, State: spi.ModelLocked, Schema: raw})
	gate := make(chan struct{})
	inner.setGate(gate)
	s := &requestScopedStore{cache: c, inner: inner}

	type result struct {
		node *schema.ModelNode
		err  error
	}
	done := make(chan result, 1)
	go func() {
		n, err := s.SchemaNode(tenantCtx(), testRef)
		done <- result{n, err}
	}()
	waitSignal(t, inner.getEntered, "SchemaNode's read to reach storage")
	c.bump(testKey())
	close(gate)
	r := <-done
	if r.err != nil {
		t.Fatalf("SchemaNode: %v", r.err)
	}
	if r.node == nil {
		t.Fatal("SchemaNode returned no node for the descriptor it read")
	}
	if _, ok := r.node.FieldsMap()["$.amount"]; !ok {
		t.Errorf("parsed node lacks $.amount: %v", r.node.FieldsMap())
	}
	if c.testHasEntry(testKey()) {
		t.Error("a fill the guard dropped was retained")
	}
}
