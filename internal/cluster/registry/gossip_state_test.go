package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

func TestGossip_StateExchangeOnJoin(t *testing.T) {
	ctx := context.Background()
	r1, err := registry.NewGossip(registry.GossipConfig{NodeID: "st-1", NodeAddr: "localhost:18190", BindAddr: "127.0.0.1", BindPort: 0, StabilityWindow: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	captureAddr(r1)
	defer r1.Deregister(ctx, "st-1")

	c1 := platformclock.NewTest("st-1", nil)
	platformclock.Attach(c1, r1, r1)
	// A setting made through the service is shareable. Its broadcast is
	// dropped here, so the joiner can only learn it by the state exchange.
	svc := platformclock.NewService(c1, nil, nil, func(platformclock.Setting) {}, time.Second)
	rate := int64(2_000_000)
	if _, err := svc.Change(ctx, platformclock.Change{RatePPM: &rate}); err != nil {
		t.Fatal(err)
	}
	want := c1.Setting()
	if err := r1.Register(ctx, "st-1", "localhost:18190"); err != nil {
		t.Fatal(err)
	}

	r2, err := registry.NewGossip(registry.GossipConfig{NodeID: "st-2", NodeAddr: "localhost:18191", BindAddr: "127.0.0.1", BindPort: 0, Seeds: []string{addrOf(r1)}, StabilityWindow: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Deregister(ctx, "st-2")
	c2 := platformclock.NewTest("st-2", nil)
	platformclock.Attach(c2, r2, r2)
	if err := r2.Register(ctx, "st-2", "localhost:18191"); err != nil {
		t.Fatal(err)
	}
	if got := c2.Setting(); got != want {
		t.Fatalf("joiner setting = %+v, want %+v", got, want)
	}
}

func TestGossip_NoStateExchangeByDefault(t *testing.T) {
	r, err := registry.NewGossip(registry.GossipConfig{NodeID: "st-3", NodeAddr: "localhost:18192", BindAddr: "127.0.0.1", BindPort: 0, StabilityWindow: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.LocalStateForTest(); got != nil {
		t.Fatalf("LocalState = %q, want nil", got)
	}
}
