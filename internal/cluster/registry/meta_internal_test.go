package registry

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"

	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

// TestCheckIdentityFits_MeasuresLongestState pins that the size check measures
// the metadata with the longest state there is, so an identity that passes at
// startup still fits when the node turns UNHEALTHY. The identity is padded to
// the exact boundary of a NORMAL node.
func TestCheckIdentityFits_MeasuresLongestState(t *testing.T) {
	longest := lifecycle.State("")
	for _, s := range lifecycle.States() {
		if len(s) > len(longest) {
			longest = s
		}
	}
	if longest != lifecycle.Unhealthy {
		t.Fatalf("the longest state is %s, not UNHEALTHY; the size check measures the wrong one", longest)
	}

	largest := listVersion{Epoch: math.MaxInt64, Seq: math.MaxUint64}
	cfg := GossipConfig{NodeAddr: "http://n.test:8080"}
	framing, err := marshalMeta(cfg, largest, lifecycle.Normal)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NodeID = strings.Repeat("n", memberlist.MetaMaxSize-len(framing))
	atNormal, err := marshalMeta(cfg, largest, lifecycle.Normal)
	if err != nil {
		t.Fatal(err)
	}
	if len(atNormal) != memberlist.MetaMaxSize {
		t.Fatalf("padded metadata is %d bytes at NORMAL, want exactly %d", len(atNormal), memberlist.MetaMaxSize)
	}
	if err := checkIdentityFits(cfg); err == nil {
		t.Fatal("an identity that fits only while NORMAL was accepted; the node would vanish when it turns UNHEALTHY")
	}

	// The identity budget the help topic states: what the three settings
	// together may take once the framing, the longest version and the longest
	// state are paid for. Each setting is one byte here, so that the optional
	// gRPC address's key is counted in the framing.
	oneByteEach, err := marshalMeta(GossipConfig{NodeID: "i", NodeAddr: "a", GRPCNodeAddr: "g"}, largest, lifecycle.Unhealthy)
	if err != nil {
		t.Fatal(err)
	}
	framingBytes := len(oneByteEach) - 3
	t.Logf("framing with the longest version and state: %d bytes; identity budget: %d bytes",
		framingBytes, memberlist.MetaMaxSize-framingBytes)
}

func TestNewGossip_MetadataStartsStarting(t *testing.T) {
	g := startInternalGossip(t, "meta-starting", time.Hour)
	nm, err := parseMeta(g.delegate.NodeMeta(0))
	if err != nil {
		t.Fatal(err)
	}
	if nm.State != lifecycle.Starting {
		t.Fatalf("a new node advertises state %q, want STARTING", nm.State)
	}
}

// storedMeta is the metadata the node currently advertises.
func storedMeta(t *testing.T, g *Gossip) nodeMeta {
	t.Helper()
	nm, err := parseMeta(g.delegate.NodeMeta(0))
	if err != nil {
		t.Fatal(err)
	}
	return nm
}

func TestSetState_PairsWithCurrentTagVersion(t *testing.T) {
	g := startInternalGossip(t, "meta-pairs", time.Hour)

	if err := g.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatal(err)
	}
	v1, _ := g.tags.ownList()
	if nm := storedMeta(t, g); nm.Tags != v1 || nm.State != lifecycle.Starting {
		t.Fatalf("after UpdateTags: (%v, %s), want (%v, STARTING)", nm.Tags, nm.State, v1)
	}

	if err := g.SetState(lifecycle.Normal); err != nil {
		t.Fatal(err)
	}
	if nm := storedMeta(t, g); nm.Tags != v1 || nm.State != lifecycle.Normal {
		t.Fatalf("after SetState: (%v, %s), want (%v, NORMAL)", nm.Tags, nm.State, v1)
	}

	if err := g.UpdateTags(map[string][]string{"tenant-b": {"go"}}); err != nil {
		t.Fatal(err)
	}
	v2, _ := g.tags.ownList()
	if v2 == v1 {
		t.Fatal("the second tag set did not bump the version")
	}
	if nm := storedMeta(t, g); nm.Tags != v2 || nm.State != lifecycle.Normal {
		t.Fatalf("after the second UpdateTags: (%v, %s), want (%v, NORMAL)", nm.Tags, nm.State, v2)
	}
}

// held is what the builder holds, read under its lock.
func (b *metaBuilder) held() (listVersion, lifecycle.State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version, b.state
}

// TestSetState_ConcurrentWithUpdateTags guards against a torn version/state
// pairing under -race; a correct implementation passes it by design.
func TestSetState_ConcurrentWithUpdateTags(t *testing.T) {
	g := startInternalGossip(t, "meta-concurrent", time.Hour)

	states := []lifecycle.State{lifecycle.Normal, lifecycle.Draining}
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			if i%2 == 0 {
				if err := g.SetState(states[(i/2)%2]); err != nil {
					t.Error(err)
				}
				return
			}
			if err := g.UpdateTags(map[string][]string{"tenant-a": {fmt.Sprintf("tag-%d", i)}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	// The final version is the tag store's; the final state is the one the
	// last SetState left. The stored metadata names both: neither call
	// overwrote the other's half with a value it read before the other ran.
	finalVersion, _ := g.tags.ownList()
	_, finalState := g.meta.held()
	nm := storedMeta(t, g)
	if nm.Tags != finalVersion {
		t.Errorf("stored metadata names version %v, the tag store holds %v", nm.Tags, finalVersion)
	}
	if nm.State != finalState {
		t.Errorf("stored metadata names state %s, the last SetState left %s", nm.State, finalState)
	}
}

func TestMemberState(t *testing.T) {
	valid, err := marshalMeta(GossipConfig{NodeID: "n", NodeAddr: "http://n.test:8080"}, listVersion{Epoch: 1}, lifecycle.Draining)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := memberState(valid); !ok || s != lifecycle.Draining {
		t.Errorf("valid metadata: (%q, %v), want (DRAINING, true)", s, ok)
	}

	for name, raw := range map[string][]byte{
		"no state":      []byte(`{"id":"n","addr":"http://n.test:8080","tv":{"e":1,"s":0}}`),
		"unknown state": []byte(`{"id":"n","addr":"http://n.test:8080","tv":{"e":1,"s":0},"st":"RESTING"}`),
		"bad json":      []byte(`{"id":`),
	} {
		if s, ok := memberState(raw); ok || s != "" {
			t.Errorf("%s: (%q, %v), want (\"\", false)", name, s, ok)
		}
	}
}
