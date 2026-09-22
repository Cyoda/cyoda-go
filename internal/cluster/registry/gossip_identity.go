package registry

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hashicorp/memberlist"
)

// errDuplicateNodeID marks the join failure that means another node of the
// cluster already answers to this node's id. The id is a security parameter —
// a hand-over and its answer are sealed for the node they are sent to, by this
// id — so two nodes holding one id open each other's hand-overs. It is
// retried, because a record left behind by this node's own previous life ages
// out; what refuses the node is the id still being held when the startup
// budget runs out.
var errDuplicateNodeID = errors.New("duplicate node id")

// conflictLogMsg is what the operator reads when a duplicate is only
// witnessed; conflictRefuseMsg when a node refuses the exchange that would
// establish one.
const (
	conflictLogMsg     = "two nodes hold the same CYODA_NODE_ID"
	conflictRefuseMsg  = "refusing a cluster merge: two nodes hold the same CYODA_NODE_ID"
	conflictReportsCap = 64
)

// identityGuard is memberlist's MergeDelegate and ConflictDelegate. Both
// delegates do the same two things: refuse the exchange, and — while this node
// is still starting — record that its id is held elsewhere, which is what
// Register reads.
//
// Between them they cover the join exchange (Merge) and the membership gossip
// that follows it (Conflict), which is what two nodes starting at the same
// instant have instead of a join exchange. Neither reaches a node that has
// already started: see NotifyConflict.
type identityGuard struct {
	nodeID string
	// self is this node's own advertised gossip address — the one peers hold
	// for it, which memberlist derives and only knows once it is created. A
	// delegate has to be installed before that, and an exchange a peer starts
	// can reach one in between, so selfAddr holds such a caller until the
	// address is known rather than judging a record against nothing. It is
	// written once, before ready is closed.
	self  string
	ready chan struct{}
	once  sync.Once

	mu sync.Mutex
	// serving is set once Register has returned successfully. From then on
	// nothing reads found, and nothing writes it either: a node that is
	// already serving does not stop because its id is contested.
	serving bool
	// found is the address of a node holding this node's id, as some exchange
	// saw it — this node's own or one a peer started. Register reads it after
	// every exchange it takes part in, and forgets it between attempts: each
	// attempt is a question about the cluster as it is then, so that a record
	// of this node's previous life that its peers have since reaped stops
	// counting against it.
	found string
	// reported holds the duplicates already logged. The key is the report,
	// the id and the two addresses, so a correctly configured cluster adds
	// nothing and one misconfigured node adds one entry — the set grows with
	// the number of misconfigured endpoints, never with the size of the
	// cluster. The cap is there only so a node flapping across ports cannot
	// grow it without bound; reaching it starts the set again, which costs a
	// repeated line rather than memory.
	reported map[string]struct{}
}

var (
	_ memberlist.MergeDelegate    = (*identityGuard)(nil)
	_ memberlist.ConflictDelegate = (*identityGuard)(nil)
)

func newIdentityGuard(nodeID string) *identityGuard {
	return &identityGuard{
		nodeID:   nodeID,
		ready:    make(chan struct{}),
		reported: make(map[string]struct{}),
	}
}

// publish hands the guard this node's advertised address and releases any
// exchange waiting for it. The first call wins: NewGossip publishes what
// memberlist derived, and publishes the empty string on the path where
// memberlist never came up, so that nothing waits for an address there will
// never be.
func (g *identityGuard) publish(addr string) {
	g.once.Do(func() {
		g.self = addr
		close(g.ready)
	})
}

// selfAddr answers this node's own advertised address, waiting for it when the
// exchange arrived before memberlist had derived one. NotifyMerge is its only
// caller: NotifyConflict runs under memberlist's node lock, which LocalNode
// takes too, so it must not block there — it uses selfIfKnown instead.
func (g *identityGuard) selfAddr() string {
	<-g.ready
	return g.self
}

// selfIfKnown answers this node's advertised address without waiting for it.
// A conflict reported before memberlist has derived one cannot be judged
// against anything, and the same conflict is reported again on the next
// gossip message carrying it.
func (g *identityGuard) selfIfKnown() (string, bool) {
	select {
	case <-g.ready:
		return g.self, true
	default:
		return "", false
	}
}

// NotifyMerge refuses the join exchange when the peer's node list shows this
// node's id at an address that is not this node's own. A non-nil error cancels
// the merge, which fails that seed and keeps the second record out of this
// node's view.
//
// It runs on both sides of a join: the node that finds its id taken, and the
// node whose id a newcomer is claiming. What they then do differs. A node that
// is still starting also records the finding, so that the attempt fails and,
// if the id is still held when its startup budget runs out, the node never
// serves. A node that is already serving refuses this one exchange, logs, and
// carries on — nothing reads the finding after Register.
func (g *identityGuard) NotifyMerge(peers []*memberlist.Node) error {
	self := g.selfAddr()
	for _, p := range peers {
		if p.Name != g.nodeID {
			continue
		}
		// A node that left or died does not hold the name: a restart under
		// the id of a node that stopped is the intended case.
		if p.State == memberlist.StateLeft || p.State == memberlist.StateDead {
			continue
		}
		addr := p.Address()
		// This node's own record, handed back by a peer that holds it.
		if addr == self {
			continue
		}
		g.note(addr)
		g.report(conflictRefuseMsg, g.nodeID, addr, self)
		return fmt.Errorf("%w: %s is held by the node at %s", errDuplicateNodeID, g.nodeID, addr)
	}
	return nil
}

// NotifyConflict fires on any node that sees a second address claim a name it
// already holds, through the membership gossip rather than a join exchange —
// which is how two nodes that started at the same instant, or two halves of a
// healing partition, learn of each other.
//
// A node can tell whether it is one of the two: memberlist takes existing from
// this node's own node map, so existing being this node's id at this node's
// address is proof. A node still starting refuses to start on that proof,
// which is what the merge delegate cannot reach. A node that is already
// serving does not stop — taking a serving node down loses availability and
// gains no correctness — so for it, and for a node that is only a witness,
// the ERROR line is the whole of the answer.
func (g *identityGuard) NotifyConflict(existing, other *memberlist.Node) {
	g.report(conflictLogMsg, existing.Name, existing.Address(), other.Address())
	self, known := g.selfIfKnown()
	if known && existing.Name == g.nodeID && existing.Address() == self {
		g.note(other.Address())
	}
}

// note records where this node's id was found, for Register to read. It is
// kept only while this node is starting: the finding is what stops a node
// before it serves, and there is nothing for it to do afterwards.
func (g *identityGuard) note(addr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.serving || g.found != "" {
		return
	}
	g.found = addr
}

// duplicate returns the address this node's id was found at, or "" if no
// exchange has found one.
func (g *identityGuard) duplicate() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.found
}

// forget drops what an earlier attempt found, so that the next one is decided
// by the exchanges it takes part in. Register calls it before every attempt.
func (g *identityGuard) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.found = ""
}

// startServing records that Register has returned and this node is about to
// serve. From here on a contested id is logged and nothing more.
func (g *identityGuard) startServing() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.serving = true
}

// report logs one duplicate-id line, at most once per report and pair of
// addresses. The gossip message carrying the record repeats, and the line must
// not.
func (g *identityGuard) report(msg, id, heldBy, claimedBy string) {
	if !g.firstReport(msg + "\x00" + id + "\x00" + heldBy + "\x00" + claimedBy) {
		return
	}
	slog.Error(msg,
		"pkg", "cluster/registry",
		"nodeId", id,
		"heldBy", heldBy,
		"claimedBy", claimedBy,
	)
}

func (g *identityGuard) firstReport(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, seen := g.reported[key]; seen {
		return false
	}
	if len(g.reported) >= conflictReportsCap {
		clear(g.reported)
	}
	g.reported[key] = struct{}{}
	return true
}
