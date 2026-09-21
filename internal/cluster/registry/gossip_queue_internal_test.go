package registry

import (
	"testing"
	"time"
)

// The broadcast queue exists before memberlist.Create starts the goroutine that
// reads it, so GetBroadcasts is usable the moment NewGossip returns and needs no
// nil guard. Assigning the queue after Create — as this constructor did — is a
// write racing that goroutine's read, and the guard it needed hid the fact.
//
// The window is microseconds wide inside the constructor, so the race detector
// does not see it on any schedule a test can force; what is testable is the
// property the fix establishes, which this pins: a broadcast queued on a node
// whose cluster is only itself is handed out by the delegate the gossip
// goroutine calls.
func TestGossip_TheBroadcastQueueIsUsableAtOnce(t *testing.T) {
	g, err := NewGossip(GossipConfig{
		NodeID:          "queue-1",
		NodeAddr:        "localhost:18190",
		BindAddr:        "127.0.0.1",
		BindPort:        18052,
		StabilityWindow: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	t.Cleanup(func() { _ = g.list.Shutdown() })

	if g.delegate.queue == nil {
		t.Fatal("the delegate has no broadcast queue although the gossip goroutine is already running")
	}
	if got := g.delegate.queue.NumNodes(); got < 1 {
		t.Errorf("NumNodes = %d, want at least this node", got)
	}
	g.Broadcast("queue-topic", []byte(`{"x":1}`))
	if msgs := g.delegate.GetBroadcasts(0, 1400); len(msgs) != 1 {
		t.Fatalf("GetBroadcasts returned %d messages, want the one that was queued", len(msgs))
	}
}
