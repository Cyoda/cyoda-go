package registry_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
)

// TestGossipRegistry_ManyTenantsStayVisible reproduces the membership defect.
// Twelve tenants with 36-character ids and one 10-character tag each need
// about 81 + 12×(36+18) = 729 bytes in the old metadata, past memberlist's
// 512; the pnode then published nil metadata and vanished from every view,
// its own included.
func TestGossipRegistry_ManyTenantsStayVisible(t *testing.T) {
	ctx := context.Background()
	r1 := startGossip(t, gossipCfg("many-1", 23946))
	r2 := startGossip(t, gossipCfg("many-2", 23947, "127.0.0.1:23946"))

	want := make(map[string][]string, 12)
	for i := range 12 {
		want[fmt.Sprintf("0b7e2c54-9a1d-4f3e-8c6b-%012d", i)] = []string{"compute-01"}
	}
	if err := r1.UpdateTags(want); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	eventually(t, 5*time.Second, "many-2 sees all twelve tenants of many-1", func() bool {
		n, ok := nodeIn(t, r2, "many-1")
		return ok && reflect.DeepEqual(n.Tags, want)
	})

	self, ok := nodeIn(t, r1, "many-1")
	if !ok {
		t.Fatal("many-1 dropped out of its own view")
	}
	if !reflect.DeepEqual(self.Tags, want) {
		t.Errorf("many-1's own tags = %v, want all twelve tenants", self.Tags)
	}

	addr, alive, err := r2.Lookup(ctx, "many-1")
	if err != nil || !alive || addr != "http://many-1.test:8080" {
		t.Errorf("Lookup(many-1) from many-2 = (%q, %v, %v), want the address, alive, nil", addr, alive, err)
	}
}

func TestGossipRegistry_TagsAreReplacedAndRemoved(t *testing.T) {
	r1 := startGossip(t, gossipCfg("repl-1", 23948))
	r2 := startGossip(t, gossipCfg("repl-2", 23949, "127.0.0.1:23948"))

	if err := r1.UpdateTags(map[string][]string{"tenant-a": {"python"}, "tenant-b": {"go"}}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	eventually(t, 5*time.Second, "repl-2 sees both tenants", func() bool {
		n, _ := nodeIn(t, r2, "repl-1")
		return len(n.Tags) == 2
	})

	// tenant-b's last cnode detached.
	if err := r1.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	eventually(t, 5*time.Second, "repl-2 sees tenant-b gone", func() bool {
		n, _ := nodeIn(t, r2, "repl-1")
		return reflect.DeepEqual(n.Tags, map[string][]string{"tenant-a": {"python"}})
	})
}

func TestNewGossip_IdentityTooLarge_RefusesToStart(t *testing.T) {
	cfg := gossipCfg("identity-too-large", 23950)
	cfg.NodeAddr = "http://" + strings.Repeat("a", 600) + ".test:8080"

	r, err := registry.NewGossip(cfg)
	if err == nil {
		_ = r.Deregister(context.Background(), cfg.NodeID)
		t.Fatal("NewGossip accepted an identity that cannot fit the cluster metadata; the pnode would run invisibly")
	}
	for _, setting := range []string{"CYODA_NODE_ID", "CYODA_NODE_ADDR", "CYODA_GRPC_NODE_ADDR"} {
		if !strings.Contains(err.Error(), setting) {
			t.Errorf("error %q does not name %s", err, setting)
		}
	}
}

func TestNewGossip_LargestIdentityThatFits_Starts(t *testing.T) {
	// The check uses the longest version there can be, so an identity that
	// passes it at startup passes it for the life of the process.
	cfg := gossipCfg("identity-fits", 23951)
	cfg.NodeAddr = "http://" + strings.Repeat("a", 300) + ".test:8080"
	r := startGossip(t, cfg)
	if _, ok := nodeIn(t, r, "identity-fits"); !ok {
		t.Fatal("the pnode is missing from its own view")
	}
}

func TestGossipRegistry_DeregisterTwice(t *testing.T) {
	r := startGossip(t, gossipCfg("dereg-twice", 23952))
	if err := r.Deregister(context.Background(), "dereg-twice"); err != nil {
		t.Fatalf("first Deregister: %v", err)
	}
	// startGossip's cleanup makes it three; none may panic.
	if err := r.Deregister(context.Background(), "dereg-twice"); err != nil {
		t.Fatalf("second Deregister: %v", err)
	}
}
