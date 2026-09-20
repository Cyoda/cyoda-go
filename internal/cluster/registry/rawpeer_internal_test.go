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

func startRawPeer(t *testing.T, name string, port int, meta []byte, seed string) *rawPeer {
	t.Helper()
	p := &rawPeer{name: name, meta: meta}
	cfg := memberlist.DefaultLANConfig()
	cfg.Name = name
	cfg.BindAddr = "127.0.0.1"
	cfg.BindPort = port
	cfg.AdvertisePort = port
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

// startInternalGossip starts one pnode. scan is its ListScanInterval: short
// where the scan is under test, an hour where a test must prove that some
// other path made the request.
func startInternalGossip(t *testing.T, id string, port int, scan time.Duration, seeds ...string) *Gossip {
	t.Helper()
	g, err := NewGossip(GossipConfig{
		NodeID:           id,
		NodeAddr:         "http://" + id + ".test:8080",
		BindAddr:         "127.0.0.1",
		BindPort:         port,
		Seeds:            seeds,
		StabilityWindow:  200 * time.Millisecond,
		ListScanInterval: scan,
	})
	if err != nil {
		t.Fatalf("NewGossip %s: %v", id, err)
	}
	t.Cleanup(func() { _ = g.Deregister(context.Background(), id) })
	if err := g.Register(context.Background(), id, ""); err != nil {
		t.Fatalf("Register %s: %v", id, err)
	}
	return g
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
