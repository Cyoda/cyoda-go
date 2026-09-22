package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestGossipRegistry_Changed_FiresOnListArrival(t *testing.T) {
	r1 := startGossip(t, gossipCfg("chg-list-1"))
	r2 := startGossipSeededBy(t, "chg-list-2", r1)
	eventually(t, 5*time.Second, "chg-list-2 sees chg-list-1", func() bool {
		_, ok := nodeIn(t, r2, "chg-list-1")
		return ok
	})
	// Let the join's own signals pass, then take the channel before the change.
	time.Sleep(500 * time.Millisecond)
	var reg contract.NodeRegistry = r2
	ch := reg.Changed()

	if err := r1.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(ch, 5*time.Second) {
		t.Fatal("a peer's list arrived and Changed was not closed")
	}

	// The way the owner's loop uses it: take the channel, look, wait. However
	// the wake-ups interleave, the list must become readable without polling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		next := reg.Changed()
		if n, _ := nodeIn(t, r2, "chg-list-1"); len(n.Tags["tenant-a"]) == 1 {
			break
		}
		if !closedWithin(next, time.Until(deadline)) {
			t.Fatal("woken, but the list never became readable")
		}
	}
}

func TestGossipRegistry_Changed_FiresOnJoinAndLeave(t *testing.T) {
	r1 := startGossip(t, gossipCfg("chg-join-1"))

	ch := r1.Changed()
	r2 := startGossipSeededBy(t, "chg-join-2", r1)
	if !closedWithin(ch, 5*time.Second) {
		t.Fatal("a peer joined and Changed was not closed")
	}

	eventually(t, 5*time.Second, "chg-join-1 sees chg-join-2", func() bool {
		_, ok := nodeIn(t, r1, "chg-join-2")
		return ok
	})
	time.Sleep(500 * time.Millisecond)
	ch = r1.Changed()
	if err := r2.Deregister(context.Background(), "chg-join-2"); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(ch, 5*time.Second) {
		t.Fatal("a peer left and Changed was not closed")
	}
}

func TestGossipRegistry_Changed_QuietClusterStaysOpen(t *testing.T) {
	r1 := startGossip(t, gossipCfg("chg-quiet-1"))
	startGossipSeededBy(t, "chg-quiet-2", r1)
	time.Sleep(time.Second) // joins and first fetches settle
	ch := r1.Changed()
	// Several scans pass; nothing changed, so nobody may be woken.
	if closedWithin(ch, time.Second) {
		t.Fatal("Changed closed with no change in the cluster view; a waiting callout would spin")
	}
}
