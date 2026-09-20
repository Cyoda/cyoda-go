package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// gossipCfg is the configuration the two-pnode tests share: loopback, a short
// stability window, an HTTP address derived from the id.
func gossipCfg(id string, port int, seeds ...string) registry.GossipConfig {
	return registry.GossipConfig{
		NodeID:           id,
		NodeAddr:         "http://" + id + ".test:8080",
		BindAddr:         "127.0.0.1",
		BindPort:         port,
		Seeds:            seeds,
		StabilityWindow:  200 * time.Millisecond,
		ListScanInterval: 200 * time.Millisecond,
	}
}

// startGossip creates and joins one pnode and leaves the cluster at cleanup.
func startGossip(t *testing.T, cfg registry.GossipConfig) *registry.Gossip {
	t.Helper()
	r, err := registry.NewGossip(cfg)
	if err != nil {
		t.Fatalf("NewGossip %s: %v", cfg.NodeID, err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background(), cfg.NodeID) })
	if err := r.Register(context.Background(), cfg.NodeID, cfg.NodeAddr); err != nil {
		t.Fatalf("Register %s: %v", cfg.NodeID, err)
	}
	return r
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", within, what)
}

// nodeIn returns the entry for id in r's view.
func nodeIn(t *testing.T, r *registry.Gossip, id string) (contract.NodeInfo, bool) {
	t.Helper()
	nodes, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, n := range nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return contract.NodeInfo{}, false
}
