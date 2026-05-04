package modelcache_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
)

// TestSubscribeLocal_FiresOnLocalMutation pins issue #174's contract.
//
// On a single-node deployment the broadcaster is nil — there is no
// gossip channel to relay schema-change events. Downstream caches
// (e.g. the path-validation negative cache) that needed those events
// were silently never notified, so an ExtendSchema on the local node
// did not invalidate them.
//
// Fix: CachingModelStore exposes SubscribeLocal so downstream caches
// can register a handler that fires for every invalidation —
// regardless of whether a broadcaster is present.
func TestSubscribeLocal_FiresOnLocalMutation(t *testing.T) {
	inner := newStubInner()
	c := modelcache.New(inner, nil /* no broadcaster — single-node */, nil, time.Minute)

	var notifications atomic.Int64
	var lastTenant atomic.Value
	var lastRef atomic.Value
	c.SubscribeLocal(func(tenant string, ref spi.ModelRef) {
		notifications.Add(1)
		lastTenant.Store(tenant)
		lastRef.Store(ref)
	})

	ref := spi.ModelRef{EntityName: "ord", ModelVersion: "1"}
	ctx := tenantCtx("tenant-A")
	if err := c.ExtendSchema(ctx, ref, spi.SchemaDelta(`[]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	if got := notifications.Load(); got != 1 {
		t.Fatalf("subscriber notifications: got %d, want 1", got)
	}
	if got := lastTenant.Load().(string); got != "tenant-A" {
		t.Errorf("tenant: got %q, want tenant-A", got)
	}
	if got := lastRef.Load().(spi.ModelRef); got != ref {
		t.Errorf("ref: got %+v, want %+v", got, ref)
	}
}

// TestSubscribeLocal_FiresOnGossipReceive pins that subscribers
// receive notifications for invalidations driven by remote gossip
// too — not just local mutations. So in multi-node mode the
// downstream cache reacts to peer ExtendSchema as well as local.
func TestSubscribeLocal_FiresOnGossipReceive(t *testing.T) {
	inner := newStubInner()
	bc := newFakeBroadcasterMC()
	c := modelcache.New(inner, bc, nil, time.Minute)

	var notifications atomic.Int64
	c.SubscribeLocal(func(tenant string, ref spi.ModelRef) {
		notifications.Add(1)
	})

	ref := spi.ModelRef{EntityName: "ord", ModelVersion: "1"}
	payload, err := modelcache.EncodeInvalidation("tenant-B", ref)
	if err != nil {
		t.Fatalf("EncodeInvalidation: %v", err)
	}
	bc.Broadcast("model.invalidate", payload)

	if got := notifications.Load(); got != 1 {
		t.Fatalf("subscriber notifications on gossip receive: got %d, want 1", got)
	}
}

// TestSubscribeLocal_MultipleSubscribers pins fan-out — every
// registered handler receives every event, in registration order.
func TestSubscribeLocal_MultipleSubscribers(t *testing.T) {
	inner := newStubInner()
	c := modelcache.New(inner, nil, nil, time.Minute)

	var aCount, bCount atomic.Int64
	c.SubscribeLocal(func(string, spi.ModelRef) { aCount.Add(1) })
	c.SubscribeLocal(func(string, spi.ModelRef) { bCount.Add(1) })

	ref := spi.ModelRef{EntityName: "ord", ModelVersion: "1"}
	if err := c.ExtendSchema(tenantCtx("tenant-A"), ref, spi.SchemaDelta(`[]`)); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}

	if a, b := aCount.Load(), bCount.Load(); a != 1 || b != 1 {
		t.Errorf("subscribers: a=%d b=%d, want a=1 b=1", a, b)
	}
}

// --- helpers ----------------------------------------------------------

type stubInnerMC struct{}

func newStubInner() *stubInnerMC { return &stubInnerMC{} }

func (s *stubInnerMC) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return nil, nil
}
func (s *stubInnerMC) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return nil, nil
}
func (s *stubInnerMC) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *stubInnerMC) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *stubInnerMC) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *stubInnerMC) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *stubInnerMC) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *stubInnerMC) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *stubInnerMC) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *stubInnerMC) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*stubInnerMC)(nil)

type fakeBroadcasterMC struct {
	handlers map[string][]func([]byte)
}

func newFakeBroadcasterMC() *fakeBroadcasterMC {
	return &fakeBroadcasterMC{handlers: make(map[string][]func([]byte))}
}
func (b *fakeBroadcasterMC) Broadcast(topic string, payload []byte) {
	for _, h := range b.handlers[topic] {
		h(payload)
	}
}
func (b *fakeBroadcasterMC) Subscribe(topic string, h func([]byte)) {
	b.handlers[topic] = append(b.handlers[topic], h)
}

var _ spi.ClusterBroadcaster = (*fakeBroadcasterMC)(nil)

func tenantCtx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		Tenant: spi.Tenant{ID: spi.TenantID(tenant)},
	})
}
