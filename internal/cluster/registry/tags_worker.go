package registry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hashicorp/memberlist"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// topicTags carries one pnode's complete tag list to a peer, and
// topicTagsRequest asks a peer for its list, over memberlist's reliable
// channel. memberlist delivers reliable user messages and gossip broadcasts to
// the same NotifyMsg, so they use the topic framing of Broadcast/Subscribe.
const (
	topicTags        = "cluster.tags"
	topicTagsRequest = "cluster.tags.request"
)

const (
	// tagEventQueueDepth bounds what memberlist's callbacks may leave for the
	// worker. A full queue drops the event; the scan makes it good.
	tagEventQueueDepth = 256
	// metaBroadcastTimeout bounds the wait for the metadata broadcast. On a
	// timeout the broadcast stays queued in memberlist and still goes out.
	metaBroadcastTimeout = 5 * time.Second
)

// tagListMsg is the payload of topicTags.
type tagListMsg struct {
	NodeID  string              `json:"n"`
	Version listVersion         `json:"v"`
	Tags    map[string][]string `json:"t,omitempty"`
}

// tagRequestMsg is the payload of topicTagsRequest: "send me your list".
type tagRequestMsg struct {
	From string `json:"from"`
}

type tagEventKind int

const (
	evList tagEventKind = iota
	evRequest
	evMember // a member joined or changed its metadata: look at it
	evLeave
)

type tagEvent struct {
	kind    tagEventKind
	node    string // evMember, evLeave: the member's name
	payload []byte // evList, evRequest: a copy of the message payload
}

// tagEvents is everything memberlist's callbacks are allowed to do: copy what
// they were given — into the directory, or into the worker's queue — and
// return. No method blocks and none calls into memberlist. NotifyJoin,
// NotifyUpdate and NotifyLeave run under memberlist's node lock, where a send
// stalls all membership processing for up to the TCP timeout and Members or
// UpdateNode deadlocks.
type tagEvents struct {
	self   string
	dir    *directory
	signal *common.ChangeSignal
	ch     chan tagEvent
}

var _ memberlist.EventDelegate = (*tagEvents)(nil)

func newTagEvents(self string, dir *directory, signal *common.ChangeSignal) *tagEvents {
	return &tagEvents{self: self, dir: dir, signal: signal, ch: make(chan tagEvent, tagEventQueueDepth)}
}

func (q *tagEvents) offer(ev tagEvent) {
	select {
	case q.ch <- ev:
	default:
	}
}

// The directory first, then the nudge: the nudge may be dropped, the directory
// entry cannot. A metadata update does not fire Changed: the list that
// follows it does, when it arrives.
func (q *tagEvents) NotifyJoin(n *memberlist.Node) {
	q.dir.set(n)
	if n.Name != q.self {
		q.signal.Fire()
	}
	q.offer(tagEvent{kind: evMember, node: n.Name})
}

func (q *tagEvents) NotifyUpdate(n *memberlist.Node) {
	q.dir.set(n)
	q.offer(tagEvent{kind: evMember, node: n.Name})
}

func (q *tagEvents) NotifyLeave(n *memberlist.Node) {
	q.dir.remove(n.Name)
	if n.Name != q.self {
		q.signal.Fire()
	}
	q.offer(tagEvent{kind: evLeave, node: n.Name})
}

// onList is the topicTags handler. memberlist reuses the buffer it passes to
// NotifyMsg, so the payload is copied before the handler returns.
func (q *tagEvents) onList(payload []byte) {
	q.offer(tagEvent{kind: evList, payload: bytes.Clone(payload)})
}

// onRequest is the topicTagsRequest handler.
func (q *tagEvents) onRequest(payload []byte) {
	q.offer(tagEvent{kind: evRequest, payload: bytes.Clone(payload)})
}

func encodeTagMsg(topic string, v any) []byte {
	payload, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal tag list message",
			"pkg", "cluster/registry", "topic", topic, "err", err)
		return nil
	}
	return encodeTopicMsg(topic, payload)
}

// runTagWorker is the one goroutine that stores lists and starts sends. It is
// started after memberlist.Create returns, so g.list is set before it runs.
func (g *Gossip) runTagWorker() {
	defer close(g.done)
	scan := time.NewTicker(g.cfg.ListScanInterval)
	defer scan.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-g.publish:
			g.publishOwnList()
		case ev := <-g.events.ch:
			g.handleTagEvent(ev)
		case <-scan.C:
			g.scanLists()
		}
	}
}

func (g *Gossip) handleTagEvent(ev tagEvent) {
	switch ev.kind {
	case evList:
		g.handleList(ev.payload)
	case evRequest:
		g.handleRequest(ev.payload)
	case evMember:
		g.handleMember(ev.node)
	case evLeave:
		g.handleLeave(ev.node)
	}
}

// handleMember runs for a pnode that joined or changed its metadata: if the
// list held for it is not the one it announces, ask for it. Events about this
// pnode are ignored — NotifyJoin fires for it inside memberlist.Create and
// NotifyUpdate inside every UpdateNode.
func (g *Gossip) handleMember(name string) {
	if name == g.cfg.NodeID {
		return
	}
	m, announced, ok := g.announced(name)
	if !ok {
		return
	}
	if !g.tags.current(name, announced) {
		g.requestList(m)
	}
}

func (g *Gossip) handleLeave(name string) {
	if name == g.cfg.NodeID {
		return
	}
	g.tags.drop(name)
}

func (g *Gossip) handleRequest(payload []byte) {
	var req tagRequestMsg
	if err := json.Unmarshal(payload, &req); err != nil {
		slog.Warn("malformed tag list request",
			"pkg", "cluster/registry", "size", len(payload), "err", err)
		return
	}
	if req.From == g.cfg.NodeID {
		return
	}
	m, _, ok := g.announced(req.From)
	if !ok {
		return // the requester is not an announced member; its next event asks again
	}
	version, tags := g.tags.ownList()
	g.sendAsync(m, "list", encodeTagMsg(topicTags, tagListMsg{NodeID: g.cfg.NodeID, Version: version, Tags: tags}))
}

func (g *Gossip) requestList(m *memberlist.Node) {
	g.sendAsync(m, "request", encodeTagMsg(topicTagsRequest, tagRequestMsg{From: g.cfg.NodeID}))
}

// scanLists is the floor under the events: it compares every alive member's
// announced version with the list held and fetches wherever they differ, and
// it forgets the lists of pnodes that are gone. It is a full scan, not a list
// of outstanding requests, because an event dropped from a full queue leaves
// no request behind.
func (g *Gossip) scanLists() {
	alive := make(map[string]struct{})
	for _, m := range g.dir.all() {
		if m.Name == g.cfg.NodeID {
			continue
		}
		alive[m.Name] = struct{}{}
		nm, err := parseMeta(m.Meta)
		if err != nil {
			continue
		}
		if !g.tags.current(m.Name, nm.Tags) {
			g.requestList(m)
		}
	}
	g.tags.retain(alive)
}

// ScanIntervalFor returns the scan interval that suits a callout patience:
// half of it, so a list lost together with its events is fetched while a
// callout can still wait for it, within 100 ms … 1 s. With waiting disabled
// it is 1 s. — below a 200 ms patience the floor wins and the scan is no
// faster than the wait; the event path still fetches at once.
func ScanIntervalFor(patience time.Duration) time.Duration {
	const (
		shortest = 100 * time.Millisecond
		longest  = time.Second
	)
	if patience <= 0 {
		return longest
	}
	return min(max(patience/2, shortest), longest)
}

// publishOwnList re-advertises the metadata, which now names the new version,
// and sends the list to every other alive pnode. Neither waits on the worker.
func (g *Gossip) publishOwnList() {
	version, tags := g.tags.ownList()
	go func() {
		if err := g.list.UpdateNode(metaBroadcastTimeout); err != nil {
			slog.Debug("metadata broadcast still queued",
				"pkg", "cluster/registry", "nodeId", g.cfg.NodeID, "err", err)
		}
	}()
	msg := encodeTagMsg(topicTags, tagListMsg{NodeID: g.cfg.NodeID, Version: version, Tags: tags})
	peers := 0
	for _, m := range g.dir.all() {
		if m.Name == g.cfg.NodeID {
			continue
		}
		peers++
		g.sendAsync(m, "list", msg)
	}
	slog.Debug("published tag list",
		"pkg", "cluster/registry", "nodeId", g.cfg.NodeID,
		"seq", version.Seq, "tenants", len(tags), "peers", peers)
}

// sendAsync sends one reliable message without holding up the caller: a dial
// to a pnode that has just died hangs for memberlist's TCP timeout. to is a
// copy from the directory, never a pointer into memberlist's node map.
func (g *Gossip) sendAsync(to *memberlist.Node, kind string, msg []byte) {
	if msg == nil {
		return
	}
	go func() {
		err := g.list.SendReliable(to, msg)
		if err == nil {
			return
		}
		select {
		case <-g.stop:
			return // shutting down; the send was never going to matter
		default:
		}
		g.metrics.sendFailed(kind)
		slog.Warn("failed to send tag list message; the peer will fetch it",
			"pkg", "cluster/registry", "peer", to.Name, "msg", kind, "err", err)
	}()
}

func (g *Gossip) handleList(payload []byte) {
	var msg tagListMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("malformed tag list message",
			"pkg", "cluster/registry", "size", len(payload), "err", err)
		return
	}
	m, announced, ok := g.announced(msg.NodeID)
	stored := g.tags.put(msg.NodeID, msg.Version, msg.Tags, announced, ok)
	slog.Debug("tag list received",
		"pkg", "cluster/registry", "peer", msg.NodeID,
		"seq", msg.Version.Seq, "tenants", len(msg.Tags), "stored", stored)
	// Still not the announced list: fetch at once — but only within the
	// announced epoch. A list of another epoch means this pnode's view of the
	// sender's metadata is behind; the answer to a fetch would be refused the
	// same way, so the metadata event (or the scan) fetches instead.
	if ok && msg.NodeID != g.cfg.NodeID && msg.Version.Epoch == announced.Epoch && !g.tags.current(msg.NodeID, announced) {
		g.requestList(m)
	}
}

// member returns a copy of the alive member called name.
func (g *Gossip) member(name string) (*memberlist.Node, bool) {
	return g.dir.get(name)
}

// announced returns the alive member called name and the list version its
// metadata announces. ok is false for a pnode that is not alive or whose
// metadata does not parse.
func (g *Gossip) announced(name string) (*memberlist.Node, listVersion, bool) {
	m, ok := g.member(name)
	if !ok {
		return nil, listVersion{}, false
	}
	nm, err := parseMeta(m.Meta)
	if err != nil {
		return nil, listVersion{}, false
	}
	return m, nm.Tags, true
}
