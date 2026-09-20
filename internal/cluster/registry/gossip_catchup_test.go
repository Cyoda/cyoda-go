package registry_test

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestGossipRegistry_LateJoinerFetchesList(t *testing.T) {
	r1 := startGossip(t, gossipCfg("late-1", 24954))
	want := map[string][]string{"tenant-a": {"ml", "python"}}
	if err := r1.UpdateTags(want); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	// late-2 was not a member when the list was sent.
	r2 := startGossip(t, gossipCfg("late-2", 24955, "127.0.0.1:24954"))
	eventually(t, 5*time.Second, "the late joiner holds late-1's list", func() bool {
		n, ok := nodeIn(t, r2, "late-1")
		return ok && reflect.DeepEqual(n.Tags, want)
	})
}

// TestGossipRegistry_RestartUnderSameID: the Helm chart's normal case. The
// second life has a new epoch and starts its seq again; its list must replace
// the first life's even though its seq is lower.
func TestGossipRegistry_RestartUnderSameID(t *testing.T) {
	ctx := context.Background()
	r1 := startGossip(t, gossipCfg("restart-1", 24956))
	first := startGossip(t, gossipCfg("restart-2", 24957, "127.0.0.1:24956"))
	for _, tags := range []map[string][]string{{"t": {"a"}}, {"t": {"a", "b"}}, {"t": {"old"}}} {
		if err := first.UpdateTags(tags); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, "restart-1 holds the first life's last list", func() bool {
		n, _ := nodeIn(t, r1, "restart-2")
		return reflect.DeepEqual(n.Tags, map[string][]string{"t": {"old"}})
	})

	if err := first.Deregister(ctx, "restart-2"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	// Same id, another address, as a rescheduled pod has.
	second := startGossip(t, gossipCfg("restart-2", 24958, "127.0.0.1:24956"))
	if err := second.UpdateTags(map[string][]string{"t": {"new"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 8*time.Second, "restart-1 holds the second life's list", func() bool {
		n, ok := nodeIn(t, r1, "restart-2")
		return ok && reflect.DeepEqual(n.Tags, map[string][]string{"t": {"new"}})
	})
}
