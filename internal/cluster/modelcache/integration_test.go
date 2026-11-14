package modelcache_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// fakeBroadcaster delivers published messages to every subscriber
// synchronously, mirroring in-process multi-node semantics.
type fakeBroadcaster struct {
	handlers map[string][]func([]byte)
}

func newFakeBroadcaster() *fakeBroadcaster {
	return &fakeBroadcaster{handlers: make(map[string][]func([]byte))}
}

func (b *fakeBroadcaster) Broadcast(topic string, payload []byte) {
	hs := append([]func([]byte){}, b.handlers[topic]...)
	for _, h := range hs {
		h(payload)
	}
}

func (b *fakeBroadcaster) Subscribe(topic string, h func([]byte)) {
	b.handlers[topic] = append(b.handlers[topic], h)
}

func withTenantContext(ctx context.Context, tenant string) context.Context {
	uc := &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant)}}
	return spi.WithUserContext(ctx, uc)
}

func TestCache_GossipInvalidation_DropsOnPeer(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}

	bc := newFakeBroadcaster()
	innerA := &stubStore{desc: lockedDescriptor(ref, "")}
	innerB := &stubStore{desc: lockedDescriptor(ref, "")}
	clk := &manualClock{now: time.Now()}

	nodeA := newModelStore(t, innerA, bc, clk, time.Hour)
	nodeB := newModelStore(t, innerB, bc, clk, time.Hour)

	ctx := withTenantContext(context.Background(), "t1")
	_, _ = nodeA.Get(ctx, ref)
	_, _ = nodeB.Get(ctx, ref)
	if innerA.getCount() != 1 || innerB.getCount() != 1 {
		t.Fatalf("setup: expected 1 Get each, got A=%d B=%d", innerA.getCount(), innerB.getCount())
	}

	// Mutate on A; gossip fires; B's entry drops.
	if err := nodeA.Lock(ctx, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	_, _ = nodeB.Get(ctx, ref)
	if innerB.getCount() != 2 {
		t.Errorf("expected B.Get to reload after gossip invalidation, got %d", innerB.getCount())
	}
}

func TestCache_GossipInvalidation_ExtendSchemaOnPeer(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "2"}

	bc := newFakeBroadcaster()
	innerA := &stubStore{desc: lockedDescriptor(ref, "")}
	innerB := &stubStore{desc: lockedDescriptor(ref, "")}
	clk := &manualClock{now: time.Now()}

	nodeA := newModelStore(t, innerA, bc, clk, time.Hour)
	nodeB := newModelStore(t, innerB, bc, clk, time.Hour)

	ctx := withTenantContext(context.Background(), "t1")
	_, _ = nodeA.Get(ctx, ref)
	_, _ = nodeB.Get(ctx, ref)

	if err := nodeA.ExtendSchema(ctx, ref, spi.SchemaDelta(`[]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	_, _ = nodeB.Get(ctx, ref)
	if innerB.getCount() != 2 {
		t.Errorf("expected B reload after peer ExtendSchema, got %d", innerB.getCount())
	}
}

func TestCache_GossipInvalidation_TenantScoped(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	bc := newFakeBroadcaster()
	innerA := &stubStore{desc: lockedDescriptor(ref, "")}
	innerB := &stubStore{desc: lockedDescriptor(ref, "")}
	clk := &manualClock{now: time.Now()}
	nodeA := newModelStore(t, innerA, bc, clk, time.Hour)
	nodeB := newModelStore(t, innerB, bc, clk, time.Hour)

	ctxA := withTenantContext(context.Background(), "tenant-A")
	ctxB := withTenantContext(context.Background(), "tenant-B")

	_, _ = nodeA.Get(ctxA, ref)
	_, _ = nodeB.Get(ctxB, ref)

	// Mutate on tenant A; tenant B's identical ref on the peer must NOT drop.
	if err := nodeA.Lock(ctxA, ref); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	_, _ = nodeB.Get(ctxB, ref)
	if innerB.getCount() != 1 {
		t.Errorf("tenant isolation broken: B reloaded after A's mutation; inner.gets=%d", innerB.getCount())
	}
}
