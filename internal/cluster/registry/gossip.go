package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// GossipConfig holds the configuration for a gossip-based NodeRegistry.
type GossipConfig struct {
	NodeID          string
	NodeAddr        string
	// GRPCNodeAddr is the gRPC endpoint (host:port) this node advertises to peers.
	// Empty means peers will derive the gRPC addr from NodeAddr's host + their local gRPC port.
	GRPCNodeAddr    string
	BindAddr        string
	BindPort        int
	Seeds           []string
	StabilityWindow time.Duration
	SecretKey       []byte
}

// nodeMeta is serialized as JSON in memberlist node metadata.
type nodeMeta struct {
	ID       string              `json:"id"`
	Addr     string              `json:"addr"`
	GRPCAddr string              `json:"grpcAddr,omitempty"`
	Tags     map[string][]string `json:"tags,omitempty"`
}

// Gossip is a NodeRegistry backed by hashicorp/memberlist.
type Gossip struct {
	cfg      GossipConfig
	list     *memberlist.Memberlist
	delegate *gossipDelegate
	mu       sync.Mutex
	meta     nodeMeta
}

var _ contract.NodeRegistry = (*Gossip)(nil)

// NewGossip creates a new gossip-based registry. It starts the memberlist
// listener but does not join any cluster — call Register to join seeds.
func NewGossip(cfg GossipConfig) (*Gossip, error) {
	nm := nodeMeta{ID: cfg.NodeID, Addr: cfg.NodeAddr, GRPCAddr: cfg.GRPCNodeAddr}
	metaBytes, err := json.Marshal(nm)
	if err != nil {
		return nil, fmt.Errorf("marshal node metadata: %w", err)
	}

	del := &gossipDelegate{
		meta: metaBytes,
		subs: make(map[string][]func([]byte)),
	}
	g := &Gossip{cfg: cfg, delegate: del, meta: nm}

	mlCfg := memberlist.DefaultLANConfig()
	mlCfg.Name = cfg.NodeID
	mlCfg.BindAddr = cfg.BindAddr
	mlCfg.BindPort = cfg.BindPort
	mlCfg.AdvertisePort = cfg.BindPort
	mlCfg.SecretKey = cfg.SecretKey
	mlCfg.Delegate = del
	mlCfg.LogOutput = &slogWriter{logger: slog.Default()}

	list, err := memberlist.Create(mlCfg)
	if err != nil {
		return nil, fmt.Errorf("create memberlist: %w", err)
	}
	g.list = list

	// The broadcast queue needs a NumNodes callback; wire it up now that the
	// memberlist exists. Retransmit multiplier follows the memberlist default.
	del.queue = &memberlist.TransmitLimitedQueue{
		NumNodes:       list.NumMembers,
		RetransmitMult: mlCfg.RetransmitMult,
	}

	slog.Info("gossip registry created",
		"pkg", "cluster/registry",
		"nodeId", cfg.NodeID,
		"bindAddr", cfg.BindAddr,
		"bindPort", cfg.BindPort,
	)

	return g, nil
}

// Register joins the cluster seeds. If no seeds are configured, the node
// proceeds as a cluster of one. Self-addresses are filtered from the seed list.
//
// The retry loop and stability-window wait are bounded by ctx — callers set
// the join deadline via context.WithTimeout (typically cfg.StartupTimeout).
// A nil/background context makes the retry loop unbounded; pass a deadline.
func (g *Gossip) Register(ctx context.Context, _ string, _ string) error {
	seeds := g.filterSelf(g.cfg.Seeds)
	if len(seeds) == 0 {
		slog.Info("no seeds configured, proceeding as cluster of one",
			"pkg", "cluster/registry",
			"nodeId", g.cfg.NodeID,
		)
		return nil
	}

	const (
		initialBackoff = 500 * time.Millisecond
		maxBackoff     = 10 * time.Second
	)

	start := time.Now()
	backoff := initialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("join seeds after %v: %w", time.Since(start), err)
		}
		_, err := g.list.Join(seeds)
		if err == nil {
			break
		}
		slog.Warn("failed to join seeds, retrying",
			"pkg", "cluster/registry",
			"nodeId", g.cfg.NodeID,
			"err", err,
			"backoff", backoff,
		)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("join seeds after %v: %w", time.Since(start), ctx.Err())
		case <-timer.C:
		}
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}

	// Wait for the stability window to allow gossip convergence.
	// Poll every 200ms; only proceed when the member count is stable for the
	// full window duration. Cancel early if ctx expires.
	if g.cfg.StabilityWindow > 0 {
		const pollInterval = 200 * time.Millisecond
		lastCount := g.list.NumMembers()
		stableSince := time.Now()
		for time.Since(stableSince) < g.cfg.StabilityWindow {
			timer := time.NewTimer(pollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("stability-window wait aborted after %v: %w", time.Since(start), ctx.Err())
			case <-timer.C:
			}
			current := g.list.NumMembers()
			if current != lastCount {
				lastCount = current
				stableSince = time.Now()
			}
		}
	}

	slog.Info("joined cluster",
		"pkg", "cluster/registry",
		"nodeId", g.cfg.NodeID,
		"seeds", seeds,
		"members", g.list.NumMembers(),
	)

	return nil
}

// Lookup returns the address and alive status for the given nodeID by scanning
// the memberlist members. If the node is not found, alive is false.
func (g *Gossip) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	for _, m := range g.list.Members() {
		if m.Name == nodeID {
			var nm nodeMeta
			if err := json.Unmarshal(m.Meta, &nm); err != nil {
				return "", false, fmt.Errorf("unmarshal metadata for %s: %w", nodeID, err)
			}
			return nm.Addr, true, nil
		}
	}
	return "", false, nil
}

// List returns all members with their decoded metadata.
func (g *Gossip) List(_ context.Context) ([]contract.NodeInfo, error) {
	members := g.list.Members()
	nodes := make([]contract.NodeInfo, 0, len(members))
	for _, m := range members {
		var nm nodeMeta
		if err := json.Unmarshal(m.Meta, &nm); err != nil {
			slog.Warn("skipping member with bad metadata",
				"pkg", "cluster/registry",
				"memberName", m.Name,
				"err", err,
			)
			continue
		}
		nodes = append(nodes, contract.NodeInfo{
			NodeID:   nm.ID,
			Addr:     nm.Addr,
			GRPCAddr: nm.GRPCAddr,
			Alive:    true,
			Tags:     nm.Tags,
		})
	}
	return nodes, nil
}

// Deregister gracefully leaves the cluster.
func (g *Gossip) Deregister(_ context.Context, _ string) error {
	if err := g.list.Leave(5 * time.Second); err != nil {
		return fmt.Errorf("leave cluster: %w", err)
	}
	if err := g.list.Shutdown(); err != nil {
		return fmt.Errorf("shutdown memberlist: %w", err)
	}
	slog.Info("left cluster",
		"pkg", "cluster/registry",
		"nodeId", g.cfg.NodeID,
	)
	return nil
}

// filterSelf removes any seed that resolves to this node's own bind address.
func (g *Gossip) filterSelf(seeds []string) []string {
	selfAddr := net.JoinHostPort(g.cfg.BindAddr, fmt.Sprintf("%d", g.cfg.BindPort))
	var filtered []string
	for _, s := range seeds {
		if s == selfAddr {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

// UpdateTags updates this node's tag metadata and pushes the change to the
// memberlist so peers pick it up via gossip.
func (g *Gossip) UpdateTags(tags map[string][]string) error {
	g.mu.Lock()
	g.meta.Tags = tags
	metaBytes, err := json.Marshal(g.meta)
	if err != nil {
		g.mu.Unlock()
		return fmt.Errorf("marshal node metadata: %w", err)
	}
	g.delegate.updateMeta(metaBytes)
	g.mu.Unlock()
	return g.list.UpdateNode(0)
}

// gossipDelegate implements memberlist.Delegate. NodeMeta carries per-node
// identity (ID, addr, tags); NotifyMsg/GetBroadcasts implement the
// topic-multiplexed broadcast channel used by Gossip.Broadcast /
// Gossip.Subscribe (see gossip_broadcast.go). LocalState / MergeRemoteState
// are no-ops — we don't use the push/pull anti-entropy state channel.
type gossipDelegate struct {
	mu   sync.RWMutex
	meta []byte

	// Broadcast multiplexing. queue holds outbound messages and is populated
	// by Gossip.Broadcast; subs maps topic -> handlers called from NotifyMsg
	// when a broadcast is delivered. queue is set in NewGossip after the
	// memberlist instance exists (it needs a NumNodes callback).
	queue  *memberlist.TransmitLimitedQueue
	subs   map[string][]func([]byte)
	subsMu sync.RWMutex
}

func (d *gossipDelegate) updateMeta(meta []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.meta = meta
}

func (d *gossipDelegate) NodeMeta(limit int) []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.meta) > limit {
		slog.Warn("node metadata exceeds limit",
			"pkg", "cluster/registry",
			"metaLen", len(d.meta),
			"limit", limit,
		)
		return nil
	}
	return d.meta
}

func (d *gossipDelegate) NotifyMsg(msg []byte) {
	topic, payload, ok := decodeTopicMsg(msg)
	if !ok {
		slog.Warn("malformed broadcast message",
			"pkg", "cluster/registry", "size", len(msg))
		return
	}
	d.subsMu.RLock()
	handlers := d.subs[topic]
	d.subsMu.RUnlock()
	for _, h := range handlers {
		h(payload)
	}
}

func (d *gossipDelegate) GetBroadcasts(overhead, limit int) [][]byte {
	if d.queue == nil {
		return nil
	}
	return d.queue.GetBroadcasts(overhead, limit)
}
func (d *gossipDelegate) LocalState(bool) []byte        { return nil }
func (d *gossipDelegate) MergeRemoteState([]byte, bool) {}

func (d *gossipDelegate) subscribe(topic string, handler func([]byte)) {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	d.subs[topic] = append(d.subs[topic], handler)
}

// slogWriter routes memberlist log output to slog at DEBUG level.
type slogWriter struct {
	logger *slog.Logger
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	w.logger.Debug(msg, "pkg", "memberlist")
	return len(p), nil
}

var _ io.Writer = (*slogWriter)(nil)
