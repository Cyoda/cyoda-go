package registry_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
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
	platformclock.Attach(c1, r1)
	// A setting made through the service is shareable. It is confirmed by
	// this node alone, so the joiner can only learn it by the state exchange.
	svc := platformclock.NewService(c1,
		func(context.Context) (time.Time, error) {
			return time.Time{}, errors.New("a rate change does not read the store's time")
		},
		func(context.Context) (spi.ScheduledTaskStore, error) {
			return nil, errors.New("a rate change does not read the scheduled tasks")
		},
		platformclock.SingleNode("st-1"), time.Second)
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
	platformclock.Attach(c2, r2)
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
	if got := r.LocalStateForTest(true); got != nil {
		t.Fatalf("LocalState = %q, want nil", got)
	}
}

func TestGossip_MergeOnlyOnJoin(t *testing.T) {
	g, err := registry.NewGossip(registry.GossipConfig{NodeID: "mj-1", NodeAddr: "localhost:18192", BindAddr: "127.0.0.1", BindPort: 0, StabilityWindow: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Deregister(context.Background(), "mj-1")
	var mu sync.Mutex
	var calls []string
	g.SetStateExchange(func() []byte { return nil }, func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, string(b))
	})
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}

	g.MergeRemoteStateForTest([]byte("x"), false)
	if c := snapshot(); len(c) != 0 {
		t.Fatalf("a periodic exchange merged: %v", c)
	}
	g.MergeRemoteStateForTest([]byte("x"), true)
	if c := snapshot(); len(c) != 1 || c[0] != "x" {
		t.Fatalf("a join exchange must merge once with x, got %v", c)
	}
}

func TestGossip_StateSentOnlyOnJoin(t *testing.T) {
	g, err := registry.NewGossip(registry.GossipConfig{NodeID: "sj-1", NodeAddr: "localhost:18193", BindAddr: "127.0.0.1", BindPort: 0, StabilityWindow: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Deregister(context.Background(), "sj-1")
	g.SetStateExchange(func() []byte { return []byte("state") }, func([]byte) {})

	if got := g.LocalStateForTest(false); got != nil {
		t.Fatalf("a periodic exchange sent %q, want nil", got)
	}
	if got := g.LocalStateForTest(true); string(got) != "state" {
		t.Fatalf("a join exchange sent %q, want %q", got, "state")
	}
}

// awaitPeerState waits until r's directory shows the member called name in
// state want.
func awaitPeerState(t *testing.T, r *registry.Gossip, name string, want lifecycle.State) {
	t.Helper()
	eventually(t, 5*time.Second, fmt.Sprintf("%s shows %s", name, want), func() bool {
		s, ok := r.MemberStateForTest(name)
		return ok && s == want
	})
}

// quietChanged returns r's Changed channel once a snapshot of it stays open
// for 200 ms, within 5 s.
func quietChanged(t *testing.T, r *registry.Gossip) <-chan struct{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ch := r.Changed()
		if !closedWithin(ch, 200*time.Millisecond) {
			return ch
		}
	}
	t.Fatal("Changed kept firing for 5s in a cluster with nothing changing")
	return nil
}

func TestSetState_ReadvertisedToPeer(t *testing.T) {
	a := startGossip(t, gossipCfg("state-adv-a"))
	b := startGossipSeededBy(t, "state-adv-b", a)
	awaitPeerState(t, a, "state-adv-b", lifecycle.Starting)

	// The join fired Changed, and so does the arrival of B's first list. Both
	// are over once A holds the list B announces; the fire for the arrival
	// follows the store by a moment, so the snapshot is retaken until one
	// stays open.
	eventually(t, 5*time.Second, "A holds the list B announces", func() bool {
		return a.HoldsAnnouncedListForTest("state-adv-b")
	})
	ch := quietChanged(t, a)

	if err := b.SetState(lifecycle.Draining); err != nil {
		t.Fatal(err)
	}
	awaitPeerState(t, a, "state-adv-b", lifecycle.Draining)

	// A state change is not a change of the cluster view callouts wait on.
	select {
	case <-ch:
		t.Fatal("a peer's state change fired Changed")
	default:
	}
}

// TestDeregister_RunsPendingReadvertise pins that a state stored right before
// Deregister still reaches the peers: the re-advertiser runs a pending poke
// before it stops. B's re-advertiser is kept busy with the NORMAL broadcast,
// so the DRAINING poke is the one still pending when Deregister stops it.
func TestDeregister_RunsPendingReadvertise(t *testing.T) {
	a := startGossip(t, gossipCfg("state-dereg-a"))
	b := startGossipSeededBy(t, "state-dereg-b", a)
	awaitPeerState(t, a, "state-dereg-b", lifecycle.Starting)

	if err := b.SetState(lifecycle.Normal); err != nil {
		t.Fatal(err)
	}
	awaitPeerState(t, a, "state-dereg-b", lifecycle.Normal)

	var sawDraining atomic.Bool
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			s, ok := a.MemberStateForTest("state-dereg-b")
			if !ok {
				return // B left
			}
			if s == lifecycle.Draining {
				sawDraining.Store(true)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := b.SetState(lifecycle.Draining); err != nil {
		t.Fatal(err)
	}
	if err := b.Deregister(context.Background(), "state-dereg-b"); err != nil {
		t.Fatal(err)
	}
	<-sampled
	if !sawDraining.Load() {
		t.Fatal("B left without its peer ever seeing DRAINING")
	}
}
