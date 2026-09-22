package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
	"go.opentelemetry.io/otel/metric"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// GossipConfig holds the configuration for a gossip-based NodeRegistry.
type GossipConfig struct {
	NodeID   string
	NodeAddr string
	// GRPCNodeAddr is the gRPC endpoint (host:port) this node advertises to peers.
	// Empty means peers will derive the gRPC addr from NodeAddr's host + their local gRPC port.
	GRPCNodeAddr    string
	BindAddr        string
	BindPort        int
	Seeds           []string
	StabilityWindow time.Duration
	SecretKey       []byte
	// ListScanInterval is how often every member's announced list version is
	// compared with the list held, as the floor under the membership events.
	// Zero means one second. app.go derives it from the callout patience with
	// ScanIntervalFor.
	ListScanInterval time.Duration
	// Meter registers the membership instruments. Nil means no instruments.
	Meter metric.Meter
}

// nodeMeta is serialized as JSON in memberlist node metadata. It holds the
// pnode's identity and the version of its tag list — nothing that grows. The
// list itself travels by reliable message (see tags_worker.go).
type nodeMeta struct {
	ID       string      `json:"id"`
	Addr     string      `json:"addr"`
	GRPCAddr string      `json:"grpcAddr,omitempty"`
	Tags     listVersion `json:"tv"`
}

func marshalMeta(cfg GossipConfig, version listVersion) ([]byte, error) {
	return json.Marshal(nodeMeta{ID: cfg.NodeID, Addr: cfg.NodeAddr, GRPCAddr: cfg.GRPCNodeAddr, Tags: version})
}

// parseMeta decodes a member's metadata. Metadata without an id is not a
// pnode of this cluster and counts as unparseable.
func parseMeta(raw []byte) (nodeMeta, error) {
	var nm nodeMeta
	if err := json.Unmarshal(raw, &nm); err != nil {
		return nodeMeta{}, err
	}
	if nm.ID == "" {
		return nodeMeta{}, errors.New("metadata carries no node id")
	}
	return nm, nil
}

// checkIdentityFits refuses an identity that cannot fit memberlist's metadata.
// The size depends only on operator settings and on the digits of the version,
// so it is measured once, with the longest version there can be.
func checkIdentityFits(cfg GossipConfig) error {
	largest, err := marshalMeta(cfg, listVersion{Epoch: math.MaxInt64, Seq: math.MaxUint64})
	if err != nil {
		return fmt.Errorf("failed to marshal node metadata: %w", err)
	}
	if len(largest) > memberlist.MetaMaxSize {
		return fmt.Errorf("node identity needs %d bytes of cluster metadata and the limit is %d: shorten CYODA_NODE_ID, CYODA_NODE_ADDR or CYODA_GRPC_NODE_ADDR",
			len(largest), memberlist.MetaMaxSize)
	}
	return nil
}

// Gossip is a NodeRegistry backed by hashicorp/memberlist.
type Gossip struct {
	cfg      GossipConfig
	list     *memberlist.Memberlist
	delegate *gossipDelegate
	identity *identityGuard
	dir      *directory
	tags     *tagStore
	events   *tagEvents
	signal   *common.ChangeSignal
	metrics  *tagMetrics

	publish     chan struct{} // one slot: this pnode's list changed
	readvertise chan struct{} // one slot: the metadata must be re-advertised
	stop        chan struct{}
	done        chan struct{} // the tag worker has returned
	advertised  chan struct{} // the re-advertiser has returned

	deregisterOnce sync.Once
	deregisterErr  error
}

var _ contract.NodeRegistry = (*Gossip)(nil)

// NewGossip creates a new gossip-based registry. It starts the memberlist
// listener but does not join any cluster — call Register to join seeds.
func NewGossip(cfg GossipConfig) (*Gossip, error) {
	if err := checkIdentityFits(cfg); err != nil {
		return nil, err
	}
	if cfg.ListScanInterval <= 0 {
		cfg.ListScanInterval = time.Second
	}
	// The epoch only has to differ between two lives of this pnode.
	epoch := time.Now().UnixNano()
	metaBytes, err := marshalMeta(cfg, listVersion{Epoch: epoch})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal node metadata: %w", err)
	}

	// This node's memberlist configuration, built before the delegate so that
	// the broadcast queue takes its retransmit multiplier from the settings this
	// node actually gossips under rather than from the library default they
	// happen to equal today.
	mlCfg := memberlist.DefaultLANConfig()
	mlCfg.Name = cfg.NodeID
	mlCfg.BindAddr = cfg.BindAddr
	mlCfg.BindPort = cfg.BindPort
	mlCfg.AdvertisePort = cfg.BindPort
	mlCfg.SecretKey = cfg.SecretKey
	mlCfg.LogOutput = &slogWriter{logger: slog.Default()}

	// The memberlist this node will hold, published once Create returns. The
	// broadcast queue needs a NumNodes callback and must exist BEFORE Create:
	// Create starts the gossip goroutine, which reads the delegate's queue, so
	// assigning it afterwards is a write racing that read. The callback answers
	// 1 — this node alone — until the memberlist is published.
	var list atomic.Pointer[memberlist.Memberlist]
	del := &gossipDelegate{
		meta: metaBytes,
		subs: make(map[string][]func([]byte)),
		queue: &memberlist.TransmitLimitedQueue{
			NumNodes: func() int {
				if ml := list.Load(); ml != nil {
					return ml.NumMembers()
				}
				return 1
			},
			RetransmitMult: mlCfg.RetransmitMult,
		},
	}
	guard := newIdentityGuard(cfg.NodeID)
	signal := common.NewChangeSignal()
	dir := newDirectory()
	events := newTagEvents(cfg.NodeID, dir, signal)
	del.subscribe(topicTags, events.onList)
	del.subscribe(topicTagsRequest, events.onRequest)
	g := &Gossip{
		cfg:         cfg,
		delegate:    del,
		identity:    guard,
		dir:         dir,
		tags:        newTagStore(cfg.NodeID, epoch, signal),
		events:      events,
		signal:      signal,
		publish:     make(chan struct{}, 1),
		readvertise: make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		advertised:  make(chan struct{}),
	}

	mlCfg.Delegate = del
	// Registered before Create: NotifyJoin fires for this pnode inside it, and
	// the directory must hold every member from the first one on.
	mlCfg.Events = events
	// Both halves of the duplicate-id check: Merge sees the join exchange,
	// Conflict the membership gossip that follows it. Neither reaches a node
	// that has already started — see gossip_identity.go.
	mlCfg.Merge = guard
	mlCfg.Conflict = guard

	// Create can accept an exchange a peer starts before it returns, and the
	// guard judges a record against the address this node advertises — which
	// memberlist derives, so it cannot be composed from the configuration:
	// under the default CYODA_GOSSIP_ADDR the bind host is empty while peers
	// hold the host's own address. The deferred publish covers the path where
	// there is never an address, so that no exchange waits forever.
	defer guard.publish("")
	ml, err := memberlist.Create(mlCfg)
	if err != nil {
		return nil, fmt.Errorf("create memberlist: %w", err)
	}
	guard.publish(ml.LocalNode().Address())
	g.list = ml
	list.Store(ml)

	metrics, err := newTagMetrics(cfg.Meter, g.outstandingLists)
	if err != nil {
		_ = ml.Shutdown()
		return nil, fmt.Errorf("failed to create membership metrics: %w", err)
	}
	g.metrics = metrics

	// Started only now: both need g.list. What the callbacks left in the queue
	// in between is still there.
	go g.runTagWorker()
	go g.runReadvertiser()

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
//
// Finding this node's id on another node fails an attempt like any other
// failure, and is retried for the same budget: the commonest cause is a
// record of this node's own previous life, left behind by a crash, which its
// peers reap within seconds. What refuses the node is the id still being held
// when the budget runs out.
func (g *Gossip) Register(ctx context.Context, _ string, _ string) error {
	seeds := g.filterSelf(g.cfg.Seeds)
	if len(seeds) == 0 {
		slog.Info("no seeds configured, proceeding as cluster of one",
			"pkg", "cluster/registry",
			"nodeId", g.cfg.NodeID,
		)
		return g.proveIdentityAndServe()
	}

	const (
		initialBackoff = 500 * time.Millisecond
		maxBackoff     = 10 * time.Second
	)

	start := time.Now()
	backoff := initialBackoff
	// The duplicate the last attempt that found one saw. It is what the
	// operator is told when the budget runs out, in preference to a bare
	// deadline: the id having been held is the actionable half of it.
	var duplicate error

	for {
		if err := ctx.Err(); err != nil {
			if duplicate != nil {
				return duplicate
			}
			return fmt.Errorf("join seeds after %v: %w", time.Since(start), err)
		}

		// Each attempt asks about the cluster as it is now. A record that has
		// since been reaped must not decide an attempt made after it went.
		g.identity.forget()

		err := g.attemptJoin(ctx, seeds, start)
		if err == nil {
			break
		}
		if errors.Is(err, errDuplicateNodeID) {
			duplicate = err
			slog.Warn("CYODA_NODE_ID is held by another node, retrying until the startup budget runs out",
				"pkg", "cluster/registry",
				"nodeId", g.cfg.NodeID,
				"err", err,
				"backoff", backoff,
			)
		} else {
			slog.Warn("failed to join seeds, retrying",
				"pkg", "cluster/registry",
				"nodeId", g.cfg.NodeID,
				"err", err,
				"backoff", backoff,
			)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}

	slog.Info("joined cluster",
		"pkg", "cluster/registry",
		"nodeId", g.cfg.NodeID,
		"seeds", seeds,
		"members", g.list.NumMembers(),
	)

	return nil
}

// attemptJoin is one pass at joining: contact the seeds, let the cluster view
// settle, and only then decide that this node's id is its own. The identity
// check is last because the wait is long enough for a second holder to have
// appeared since the join.
func (g *Gossip) attemptJoin(ctx context.Context, seeds []string, start time.Time) error {
	if err := g.joinSeeds(seeds); err != nil {
		return err
	}
	if err := g.awaitStability(ctx, start); err != nil {
		return err
	}
	return g.proveIdentityAndServe()
}

// awaitStability waits for gossip to converge: the member count must hold
// still for the whole of the configured window. It polls every 200ms and
// gives up when ctx expires.
func (g *Gossip) awaitStability(ctx context.Context, start time.Time) error {
	if g.cfg.StabilityWindow <= 0 {
		return nil
	}
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
	return nil
}

// joinSeeds contacts every seed, as memberlist's own Join does, one at a time
// so that a seed can be named when the exchange with it shows this node's id
// already held. The join succeeds when any seed answered.
//
// What the guard found is read after every exchange, whatever Join returned.
// A join that answers success is no evidence that none of its exchanges
// refused: one seed name resolves to several addresses, memberlist clears the
// errors it collected as soon as one of them answers, and a peer joining this
// node runs the same delegate on an exchange this node did not start. Only the
// seed named in the message can be the wrong one; the finding cannot.
func (g *Gossip) joinSeeds(seeds []string) error {
	var errs error
	joined := 0
	for _, seed := range seeds {
		_, err := g.list.Join([]string{seed})
		if addr := g.identity.duplicate(); addr != "" {
			return g.duplicateIDError(addr, seed)
		}
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		joined++
	}
	if joined > 0 {
		return nil
	}
	return errs
}

// proveIdentityAndServe fails when anything this attempt saw — an exchange of
// its own, one a peer started, or the membership gossip that followed —
// showed this node's id on another node. A node that cannot show the id is
// its own does not serve under it.
//
// The check and the flip to serving happen as one step, under identityGuard's
// own lock (identityGuard.proveAndServe): a finding recorded at any point up
// to this call is the one it sees. There is no separate call afterwards that
// a finding landing in between could go unread by.
func (g *Gossip) proveIdentityAndServe() error {
	if addr := g.identity.proveAndServe(); addr != "" {
		return g.duplicateIDError(addr, "")
	}
	return nil
}

// duplicateIDError is what an operator reads. It names the setting, where the
// id was found and, when the exchange was this node's own, the seed it was
// learned from. The second cause is named too: nothing separates an impostor
// from a record of this node's own previous life, which its peers keep after a
// crash because it never left, and which they hold at the old address.
func (g *Gossip) duplicateIDError(addr, seed string) error {
	where := "found in gossip this node did not start"
	if seed != "" {
		where = "learned from seed " + seed
	}
	return fmt.Errorf("%w: CYODA_NODE_ID %q is already held by the node at %s, %s; every node of a cluster needs an id of its own. A node that crashed and came back at another address is refused the same way, because that record is its own previous life: it starts once its peers have reaped it",
		errDuplicateNodeID, g.cfg.NodeID, addr, where)
}

// Lookup returns the address and alive status for the given nodeID from the
// member directory. If the node is not found, alive is false. A member whose
// metadata does not parse is reported as not alive.
func (g *Gossip) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	m, ok := g.dir.get(nodeID)
	if !ok {
		return "", false, nil
	}
	nm, err := parseMeta(m.Meta)
	if err != nil {
		// Same verdict as List: a member that cannot be read is not a pnode
		// work can be sent to. Not an error — callers answer an error with a
		// 500, and this is a 503.
		slog.Warn("member has unparseable metadata; treating it as not alive",
			"pkg", "cluster/registry",
			"memberName", m.Name,
			"err", err,
		)
		return "", false, nil
	}
	return nm.Addr, true, nil
}

// List returns all alive members with their identity and the tags held for
// them — empty until a member's list has arrived.
func (g *Gossip) List(_ context.Context) ([]contract.NodeInfo, error) {
	members := g.dir.all()
	nodes := make([]contract.NodeInfo, 0, len(members))
	for _, m := range members {
		nm, err := parseMeta(m.Meta)
		if err != nil {
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
			Tags:     g.tags.tagsOf(m.Name),
		})
	}
	return nodes, nil
}

// outstandingLists counts the alive peers whose announced list is not the one
// held. It runs on the metrics reader's goroutine and reads the directory.
func (g *Gossip) outstandingLists() int64 {
	var n int64
	for _, m := range g.dir.all() {
		if m.Name == g.cfg.NodeID {
			continue
		}
		nm, err := parseMeta(m.Meta)
		if err != nil {
			continue
		}
		if !g.tags.current(m.Name, nm.Tags) {
			n++
		}
	}
	return n
}

// Deregister stops the tag worker and the re-advertiser and gracefully leaves
// the cluster. A second call returns the first call's result.
func (g *Gossip) Deregister(_ context.Context, _ string) error {
	g.deregisterOnce.Do(func() {
		close(g.stop)
		<-g.done
		// A re-advertise in flight waits for its broadcast, so this can take
		// up to metaBroadcastTimeout. It is waited for rather than left
		// running: Leave and Shutdown below are calls into the same
		// memberlist, and two at once is what this goroutine exists to avoid.
		<-g.advertised
		g.metrics.close()
		if err := g.list.Leave(5 * time.Second); err != nil {
			g.deregisterErr = fmt.Errorf("leave cluster: %w", err)
			return
		}
		if err := g.list.Shutdown(); err != nil {
			g.deregisterErr = fmt.Errorf("shutdown memberlist: %w", err)
			return
		}
		slog.Info("left cluster",
			"pkg", "cluster/registry",
			"nodeId", g.cfg.NodeID,
		)
	})
	return g.deregisterErr
}

// Changed returns the channel closed at the next change of the cluster view.
func (g *Gossip) Changed() <-chan struct{} {
	return g.signal.Changed()
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

// UpdateTags replaces this pnode's tag list. The new version, the metadata
// that announces it and the list are one locked step; the network work is the
// worker's, so the caller never waits on a peer. A set equal to the current
// one changes nothing.
func (g *Gossip) UpdateTags(tags map[string][]string) error {
	changed, err := g.tags.setOwn(tags, func(version listVersion) error {
		metaBytes, err := marshalMeta(g.cfg, version)
		if err != nil {
			return fmt.Errorf("failed to marshal node metadata: %w", err)
		}
		g.delegate.updateMeta(metaBytes)
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		select {
		case g.publish <- struct{}{}:
		default: // a publish is pending; it reads the newest list
		}
	}
	return nil
}

// gossipDelegate implements memberlist.Delegate. NodeMeta carries per-node
// identity (id, addresses, tag list version); NotifyMsg/GetBroadcasts
// implement the topic-multiplexed broadcast channel used by Gossip.Broadcast /
// Gossip.Subscribe (see gossip_broadcast.go). LocalState / MergeRemoteState
// are no-ops — we don't use the push/pull anti-entropy state channel.
type gossipDelegate struct {
	mu   sync.RWMutex
	meta []byte

	// Broadcast multiplexing. queue holds outbound messages and is populated
	// by Gossip.Broadcast; subs maps topic -> handlers called from NotifyMsg
	// when a broadcast is delivered. queue is set in NewGossip BEFORE
	// memberlist.Create, which starts the goroutine that reads it through
	// GetBroadcasts: its NumNodes callback reads the memberlist through an
	// atomic pointer rather than the queue being assigned once one exists.
	// It is never nil on a delegate NewGossip built.
	queue  *memberlist.TransmitLimitedQueue
	subs   map[string][]func([]byte)
	subsMu sync.RWMutex
}

func (d *gossipDelegate) updateMeta(meta []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.meta = meta
}

func (d *gossipDelegate) NodeMeta(int) []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.meta
}

func (d *gossipDelegate) NotifyMsg(msg []byte) {
	topic, payload, ok := decodeTopicMsg(msg)
	if !ok {
		slog.Warn("malformed broadcast message",
			"pkg", "cluster/registry", "size", len(msg))
		return
	}
	handlers := func() []func([]byte) {
		d.subsMu.RLock()
		defer d.subsMu.RUnlock()
		return d.subs[topic]
	}()
	for _, h := range handlers {
		h(payload)
	}
}

func (d *gossipDelegate) GetBroadcasts(overhead, limit int) [][]byte {
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
