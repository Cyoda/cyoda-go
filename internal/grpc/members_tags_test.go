package grpc

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// awaitTags returns the first published snapshot in which tenant t advertises
// marker — the change the test made last, and therefore the newest membership.
func awaitTags(t *testing.T, published <-chan map[string][]string, marker string) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case tags := <-published:
			if slices.Contains(tags["t"], marker) {
				got := slices.Clone(tags["t"])
				slices.Sort(got)
				return got
			}
		case <-deadline:
			t.Fatalf("no snapshot advertising %q was published", marker)
		}
	}
}

// The tag lists this pnode tells the cluster about answer the same question
// Candidates does — can this node serve these tags — so a member that has been
// evicted counts for nothing in them either. Its registration is removed a
// moment later; until then its tags would invite a hand-over to a cnode that is
// already gone.
func TestMemberRegistry_AnEvictedMembersTagsAreNotAdvertised(t *testing.T) {
	tests := []struct {
		name     string
		alsoHere bool // a second member holds the same tag
		want     []string
	}{
		{"the evicted member held the tag alone", false, []string{"marker"}},
		{"another member holds the same tag", true, []string{"marker", "shared"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewMemberRegistry()
			published := make(chan map[string][]string, 16)
			reg.SetOnChange(func(tags map[string][]string) error {
				published <- tags
				return nil
			})
			gone := reg.Register("m-gone", "t", []string{"shared"}, noopSend, nil)
			t.Cleanup(func() { reg.Unregister(gone) })
			if tt.alsoHere {
				here := reg.Register("m-here", "t", []string{"shared"}, noopSend, nil)
				t.Cleanup(func() { reg.Unregister(here) })
			}

			gone.Evict(errors.New("stream dropped")) // evicted, not unregistered

			// Eviction is not a membership change and publishes nothing of its
			// own; the next change does, and its snapshot is what the cluster is
			// told this node can serve.
			marker := reg.Register("m-marker", "t", []string{"marker"}, noopSend, nil)
			t.Cleanup(func() { reg.Unregister(marker) })

			if got := awaitTags(t, published, "marker"); !slices.Equal(got, tt.want) {
				t.Errorf("advertised tags = %v, want %v", got, tt.want)
			}
		})
	}
}

// An older snapshot never overwrites a newer one: whatever ends up published
// last is the newest membership, even when an earlier snapshot's publish
// failed. (That a failed publish leaves the version unadvanced is the
// mechanism; it is not separately observable from here — this test only sees
// the final published snapshot.)
func TestMemberRegistry_TagPublishIsVersioned(t *testing.T) {
	reg := NewMemberRegistry()
	var mu sync.Mutex
	var published []map[string][]string
	reg.SetOnChange(func(tags map[string][]string) error {
		mu.Lock()
		defer mu.Unlock()
		// The v1 snapshot ({t:[a]}) fails to publish, whichever goroutine
		// order the scheduler picks; v2 ({t:[a,b]}) and v3 ({t:[b]}) succeed.
		if ts := tags["t"]; len(ts) == 1 && ts[0] == "a" {
			return errors.New("gossip down")
		}
		published = append(published, tags)
		return nil
	})

	m1 := reg.Register("m1", "t", []string{"a"}, noopSend, nil) // v1: fails
	m2 := reg.Register("m2", "t", []string{"b"}, noopSend, nil) // v2
	reg.Unregister(m1)                                          // v3: newest
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(published) == 0 {
		t.Fatal("nothing published")
	}
	last := published[len(published)-1]
	if got := last["t"]; len(got) != 1 || got[0] != "b" {
		t.Fatalf("last published tags = %v, want [b] (the newest membership)", got)
	}
	reg.Unregister(m2)
}

// Publishes deliver monotonically increasing versions even when goroutines
// race: drive many changes and assert the final published state is the final
// membership.
func TestMemberRegistry_TagPublishLatestWinsUnderFlap(t *testing.T) {
	reg := NewMemberRegistry()
	var mu sync.Mutex
	var last map[string][]string
	reg.SetOnChange(func(tags map[string][]string) error {
		time.Sleep(time.Millisecond) // widen the race window
		mu.Lock()
		last = tags
		mu.Unlock()
		return nil
	})
	for i := 0; i < 50; i++ {
		id := "m" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		m := reg.Register(id, "t", []string{"flap"}, noopSend, nil)
		reg.Unregister(m)
	}
	stable := reg.Register("stable", "t", []string{"stable"}, noopSend, nil)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got := last["t"]; len(got) != 1 || got[0] != "stable" {
		t.Fatalf("final published tags = %v, want [stable]", got)
	}
	reg.Unregister(stable)
}
