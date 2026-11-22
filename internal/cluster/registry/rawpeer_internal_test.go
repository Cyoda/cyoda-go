package registry

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
)

type rawPeer struct {
	name string
	list *memberlist.Memberlist

	mu       sync.Mutex
	meta     []byte
	requests []string     // From of every cluster.tags.request received
	lists    []tagListMsg // every cluster.tags received
}

var _ memberlist.Delegate = (*rawPeer)(nil)

func (p *rawPeer) NodeMeta(int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meta
}

func (p *rawPeer) NotifyMsg(b []byte) {
	topic, payload, ok := decodeTopicMsg(b)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch topic {
	case topicTagsRequest:
		var req tagRequestMsg
		if json.Unmarshal(payload, &req) == nil {
			p.requests = append(p.requests, req.From)
		}
	case topicTags:
		var msg tagListMsg
		if json.Unmarshal(payload, &msg) == nil {
			p.lists = append(p.lists, msg)
		}
	}
}

func (p *rawPeer) GetBroadcasts(int, int) [][]byte { return nil }
func (p *rawPeer) LocalState(bool) []byte          { return nil }
func (p *rawPeer) MergeRemoteState([]byte, bool)   {}

func rawMeta(t *testing.T, name string, version listVersion) []byte {
	t.Helper()
	b, err := json.Marshal(nodeMeta{ID: name, Addr: "http://" + name + ".test:8080", Tags: version})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startRawPeer(t *testing.T, name string, meta []byte, seed string) *rawPeer {
	t.Helper()
	p := &rawPeer{name: name, meta: meta}
	cfg := memberlist.DefaultLANConfig()
	cfg.Name = name
	cfg.BindAddr = "127.0.0.1"
	cfg.BindPort = 0
	cfg.AdvertisePort = 0
	cfg.Delegate = p
	cfg.LogOutput = io.Discard
	list, err := memberlist.Create(cfg)
	if err != nil {
		t.Fatalf("create raw peer %s: %v", name, err)
	}
	p.list = list
	t.Cleanup(func() { _ = list.Shutdown() })
	if _, err := list.Join([]string{seed}); err != nil {
		t.Fatalf("raw peer %s join %s: %v", name, seed, err)
	}
	return p
}

func (p *rawPeer) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *rawPeer) receivedLists() []tagListMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tagListMsg(nil), p.lists...)
}

// announce replaces the metadata and re-advertises it.
func (p *rawPeer) announce(t *testing.T, meta []byte) {
	t.Helper()
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.meta = meta
	}()
	if err := p.list.UpdateNode(2 * time.Second); err != nil {
		t.Logf("raw peer UpdateNode: %v", err)
	}
}

// send addresses the target by its loopback port rather than through
// p.list.Members(), whose nodes memberlist rewrites under its own lock.
func (p *rawPeer) send(t *testing.T, to string, toPort int, topic string, v any) {
	t.Helper()
	target := &memberlist.Node{Name: to, Addr: net.ParseIP("127.0.0.1"), Port: uint16(toPort)}
	if err := p.list.SendReliable(target, encodeTagMsg(topic, v)); err != nil {
		t.Fatalf("raw peer send to %s: %v", to, err)
	}
}

// startInternalGossip starts one pnode on the port the OS assigns it. scan is
// its ListScanInterval: short where the scan is under test, an hour where a
// test must prove that some other path made the request.
func startInternalGossip(t *testing.T, id string, scan time.Duration, seeds ...string) *Gossip {
	t.Helper()
	g, err := NewGossip(GossipConfig{
		NodeID:           id,
		NodeAddr:         "http://" + id + ".test:8080",
		BindAddr:         "127.0.0.1",
		BindPort:         0,
		Seeds:            seeds,
		StabilityWindow:  200 * time.Millisecond,
		ListScanInterval: scan,
	})
	if err != nil {
		t.Fatalf("NewGossip %s: %v", id, err)
	}
	captureGossipAddr(g)
	t.Cleanup(func() { _ = g.Deregister(context.Background(), id) })
	if err := g.Register(context.Background(), id, ""); err != nil {
		t.Fatalf("Register %s: %v", id, err)
	}
	return g
}

// gossipAddrMu guards the address and port captureGossipAddr reads from each
// node exactly once, right after NewGossip returns. memberlist's LocalNode()
// hands out a live, unlocked pointer into the node's own record, and once a
// node's UpdateTags has ever been called, its re-advertiser goroutine
// rewrites that same record's Addr/Port fields (to the same values, but a
// write nonetheless) on every re-advertise — so a live read at any later
// point races with it. Capturing once, before a test can have called
// UpdateTags, and caching it here is what gossipAddr and gossipPort read.
var (
	gossipAddrMu sync.Mutex
	gossipAddrOf = map[*Gossip]string{}
	gossipPortOf = map[*Gossip]int{}
)

func captureGossipAddr(g *Gossip) *Gossip {
	gossipAddrMu.Lock()
	defer gossipAddrMu.Unlock()
	local := g.list.LocalNode()
	gossipAddrOf[g] = local.Address()
	gossipPortOf[g] = int(local.Port)
	return g
}

// gossipAddr returns the loopback address captured for g, for a raw peer that
// must seed off it. gossipPort returns the same captured port as an int, for
// a raw peer's send calls, which address their target directly rather than
// through a seed string.
func gossipAddr(g *Gossip) string {
	gossipAddrMu.Lock()
	defer gossipAddrMu.Unlock()
	a, ok := gossipAddrOf[g]
	if !ok {
		panic("gossipAddr: address was never captured for this node; construct it through startInternalGossip or startMeteredGossip")
	}
	return a
}

func gossipPort(g *Gossip) int {
	gossipAddrMu.Lock()
	defer gossipAddrMu.Unlock()
	p, ok := gossipPortOf[g]
	if !ok {
		panic("gossipPort: port was never captured for this node; construct it through startInternalGossip or startMeteredGossip")
	}
	return p
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
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
