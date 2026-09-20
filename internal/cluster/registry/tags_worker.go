package registry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hashicorp/memberlist"
)

// topicTags carries one pnode's complete tag list to a peer over memberlist's
// reliable channel. memberlist delivers reliable user messages and gossip
// broadcasts to the same NotifyMsg, so it uses the topic framing of
// Broadcast/Subscribe.
const topicTags = "cluster.tags"

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

type tagEventKind int

const (
	evList tagEventKind = iota
)

type tagEvent struct {
	kind    tagEventKind
	payload []byte // a copy of the message payload
}

// tagEvents is everything memberlist's callbacks are allowed to do: copy what
// they were given — into the directory, or into the worker's queue — and
// return. No method blocks and none calls into memberlist. NotifyJoin,
// NotifyUpdate and NotifyLeave run under memberlist's node lock, where a send
// stalls all membership processing for up to the TCP timeout and Members or
// UpdateNode deadlocks.
type tagEvents struct {
	dir *directory
	ch  chan tagEvent
}

var _ memberlist.EventDelegate = (*tagEvents)(nil)

func newTagEvents(dir *directory) *tagEvents {
	return &tagEvents{dir: dir, ch: make(chan tagEvent, tagEventQueueDepth)}
}

func (q *tagEvents) offer(ev tagEvent) {
	select {
	case q.ch <- ev:
	default:
	}
}

func (q *tagEvents) NotifyJoin(n *memberlist.Node)   { q.dir.set(n) }
func (q *tagEvents) NotifyUpdate(n *memberlist.Node) { q.dir.set(n) }
func (q *tagEvents) NotifyLeave(n *memberlist.Node)  { q.dir.remove(n.Name) }

// onList is the topicTags handler. memberlist reuses the buffer it passes to
// NotifyMsg, so the payload is copied before the handler returns.
func (q *tagEvents) onList(payload []byte) {
	q.offer(tagEvent{kind: evList, payload: bytes.Clone(payload)})
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
	for {
		select {
		case <-g.stop:
			return
		case <-g.publish:
			g.publishOwnList()
		case ev := <-g.events.ch:
			g.handleTagEvent(ev)
		}
	}
}

func (g *Gossip) handleTagEvent(ev tagEvent) {
	switch ev.kind {
	case evList:
		g.handleList(ev.payload)
	}
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
	_, announced, ok := g.announced(msg.NodeID)
	stored := g.tags.put(msg.NodeID, msg.Version, msg.Tags, announced, ok)
	slog.Debug("tag list received",
		"pkg", "cluster/registry", "peer", msg.NodeID,
		"seq", msg.Version.Seq, "tenants", len(msg.Tags), "stored", stored)
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
