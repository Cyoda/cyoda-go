package scheduler

import (
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestRoundRobin_Cycles(t *testing.T) {
	d := NewRoundRobin()
	m := []contract.NodeInfo{{NodeID: "a"}, {NodeID: "b"}}
	got := []string{
		d.Pick(m, "a", spi.ScheduledTask{}),
		d.Pick(m, "a", spi.ScheduledTask{}),
		d.Pick(m, "a", spi.ScheduledTask{}),
	}
	// deterministic cycling over sorted member ids
	if got[0] == got[1] {
		t.Errorf("round-robin should alternate, got %v", got)
	}
	if got[0] != got[2] {
		t.Errorf("round-robin should cycle back after len(members), got %v", got)
	}
	for _, id := range got {
		if id != "a" && id != "b" {
			t.Errorf("unexpected pick %q, want one of the members", id)
		}
	}
}

func TestRoundRobin_DeterministicOrderIgnoresInputOrder(t *testing.T) {
	// Sorted member ids drive the rotation regardless of the slice order
	// passed in, so behavior is stable across gossip-ordering changes.
	unsorted := []contract.NodeInfo{{NodeID: "c"}, {NodeID: "a"}, {NodeID: "b"}}
	d1 := NewRoundRobin()
	d2 := NewRoundRobin()

	reordered := []contract.NodeInfo{{NodeID: "b"}, {NodeID: "c"}, {NodeID: "a"}}

	for i := 0; i < 3; i++ {
		p1 := d1.Pick(unsorted, "x", spi.ScheduledTask{})
		p2 := d2.Pick(reordered, "x", spi.ScheduledTask{})
		if p1 != p2 {
			t.Fatalf("iteration %d: sorted-order pick diverged: %q vs %q", i, p1, p2)
		}
	}
}

func TestRoundRobin_EmptyMembers(t *testing.T) {
	d := NewRoundRobin()
	if got := d.Pick(nil, "self", spi.ScheduledTask{}); got != "self" {
		t.Errorf("empty membership should fall back to selfID, got %q", got)
	}
}

func TestRoundRobin_ConcurrentPicksAreRaceFree(t *testing.T) {
	d := NewRoundRobin()
	m := []contract.NodeInfo{{NodeID: "a"}, {NodeID: "b"}, {NodeID: "c"}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Pick(m, "a", spi.ScheduledTask{})
		}()
	}
	wg.Wait()
}

func TestSelf_AlwaysSelf(t *testing.T) {
	if (Self{}).Pick(nil, "x", spi.ScheduledTask{}) != "x" {
		t.Error()
	}
}
