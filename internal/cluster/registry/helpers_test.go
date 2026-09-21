package registry_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// gossipCfg is the configuration the two-pnode tests share: loopback, a short
// stability window, a gossip port the OS assigns rather than a fixed one — a
// second test process on the same machine must not collide with this one on
// a literal port. A node other nodes must seed off is looked up afterwards
// through Gossip.LocalAddr (gossip_export_test.go), never guessed in advance.
func gossipCfg(id string, seeds ...string) registry.GossipConfig {
	return gossipCfgAt(id, 0, seeds...)
}

// gossipCfgAt is gossipCfg with an explicit bind port, for the rare test
// where two lives of a node must bind the identical address. The port itself
// still comes from freePort, never a literal.
func gossipCfgAt(id string, port int, seeds ...string) registry.GossipConfig {
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

// freePort returns a loopback port that was free a moment ago. Only used
// where a test needs a port in hand before a node binds it (a seed address
// configured before the cluster exists, or two nodes sharing one address); a
// port free now can be taken by the time it is bound, which is a flake to
// retry, not a literal to fall back to.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// addrMu guards nodeAddr, the address captureAddr reads from each node
// exactly once, right after NewGossip returns. memberlist's LocalNode()
// hands out a live, unlocked pointer into the node's own record, and once a
// node's UpdateTags has ever been called, its re-advertiser goroutine
// rewrites that same record's Addr/Port fields (to the same values, but a
// write nonetheless) on every re-advertise — so a live read of Gossip.LocalAddr
// at any later point races with it. Capturing once, before a test can have
// called UpdateTags, and caching it here is what addrOf reads; nothing calls
// LocalAddr again after that.
var (
	addrMu   sync.Mutex
	nodeAddr = map[*registry.Gossip]string{}
)

// captureAddr records r's bound address for addrOf and returns r unchanged,
// so a construction site can wrap its result: `r := captureAddr(mustCreate())`.
func captureAddr(r *registry.Gossip) *registry.Gossip {
	addrMu.Lock()
	defer addrMu.Unlock()
	nodeAddr[r] = r.LocalAddr()
	return r
}

// addrOf returns the address captureAddr recorded for r.
func addrOf(r *registry.Gossip) string {
	addrMu.Lock()
	defer addrMu.Unlock()
	a, ok := nodeAddr[r]
	if !ok {
		panic("addrOf: address was never captured for this node; wrap its construction in captureAddr")
	}
	return a
}

// startGossip creates and joins one pnode on the port the OS assigned it and
// leaves the cluster at cleanup.
func startGossip(t *testing.T, cfg registry.GossipConfig) *registry.Gossip {
	t.Helper()
	r, err := registry.NewGossip(cfg)
	if err != nil {
		t.Fatalf("NewGossip %s: %v", cfg.NodeID, err)
	}
	captureAddr(r)
	t.Cleanup(func() { _ = r.Deregister(context.Background(), cfg.NodeID) })
	if err := r.Register(context.Background(), cfg.NodeID, cfg.NodeAddr); err != nil {
		t.Fatalf("Register %s: %v", cfg.NodeID, err)
	}
	return r
}

// startGossipSeededBy creates and joins a pnode that seeds off r, at the
// address memberlist actually bound for r rather than a literal, and leaves
// the cluster at cleanup.
func startGossipSeededBy(t *testing.T, id string, r *registry.Gossip) *registry.Gossip {
	t.Helper()
	return startGossip(t, gossipCfg(id, addrOf(r)))
}

// newGossipAtAnotherAddress creates a pnode whose bound address is not
// notThis, and leaves the cluster at cleanup. It does not join: a caller that
// wants the join is testing what the join does with it.
//
// The retry is the point. A restart scenario is about an id coming back at a
// DIFFERENT address, and an ephemeral port is free to be handed out again the
// moment the node that held it has gone — so a second life that lands on the
// first life's port would quietly exercise the same-address case instead. The
// fixed ports these helpers replaced guaranteed the difference by construction;
// this restores that guarantee without going back to literals.
func newGossipAtAnotherAddress(t *testing.T, cfg registry.GossipConfig, notThis string) *registry.Gossip {
	t.Helper()
	const attempts = 10
	for range attempts {
		r, err := registry.NewGossip(cfg)
		if err != nil {
			t.Fatalf("NewGossip %s: %v", cfg.NodeID, err)
		}
		captureAddr(r)
		if addrOf(r) != notThis {
			t.Cleanup(func() { _ = r.Deregister(context.Background(), cfg.NodeID) })
			return r
		}
		// The OS handed back the address under test. Give it up and ask again.
		if err := r.Deregister(context.Background(), cfg.NodeID); err != nil {
			t.Fatalf("release a node that landed on the address under test: %v", err)
		}
	}
	t.Fatalf("no address other than %s in %d attempts", notThis, attempts)
	return nil
}

// startGossipAtAnotherAddress is newGossipAtAnotherAddress plus the join, for
// a caller that only needs the node to be in the cluster.
func startGossipAtAnotherAddress(t *testing.T, id string, seed *registry.Gossip, notThis string) *registry.Gossip {
	t.Helper()
	cfg := gossipCfg(id, addrOf(seed))
	r := newGossipAtAnotherAddress(t, cfg, notThis)
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
