package registry

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hashicorp/memberlist"
)

// errDuplicateNodeID marks the one join failure no retry can clear: another
// node of the cluster already answers to this node's id. The id is a security
// parameter — a hand-over and its answer are sealed for the node they are sent
// to, by this id — so two nodes holding one id open each other's hand-overs.
var errDuplicateNodeID = errors.New("duplicate node id")

// conflictLogMsg is what the operator reads when a duplicate is only
// witnessed; conflictRefuseMsg when a node refuses the exchange that would
// establish one.
const (
	conflictLogMsg     = "two nodes hold the same CYODA_NODE_ID"
	conflictRefuseMsg  = "refusing a cluster merge: two nodes hold the same CYODA_NODE_ID"
	conflictReportsCap = 64
)

// identityGuard is memberlist's MergeDelegate and ConflictDelegate. As the
// merge delegate it refuses the join exchange that would let a second node
// answer to this node's id, which fails the join and stops this node starting.
// As the conflict delegate it reports a duplicate this node only witnesses.
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
	// found is the address of a node holding this node's id, as some exchange
	// saw it — this node's own or one a peer started. It is never cleared: an
	// id shown to be held twice stays held twice, and Register reads it after
	// every exchange it takes part in.
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
// caller: NotifyConflict runs under memberlist's node lock and must not block
// there, and it is handed both addresses it needs.
func (g *identityGuard) selfAddr() string {
	<-g.ready
	return g.self
}

// NotifyMerge refuses the join exchange when the peer's node list shows this
// node's id at an address that is not this node's own. A non-nil error cancels
// the merge, which fails that seed and, through Register, stops this node
// before it serves anything.
//
// It runs on both sides of a join: the node that finds its id taken, and the
// node whose id a newcomer is claiming. Neither can tell which it is, and both
// refusing is the right answer — the id really is held twice, and a cluster is
// better off with neither node than with two that can open each other's
// hand-overs.
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
// already holds, including where the merge delegate cannot fire: no seeds
// configured, two nodes starting at the same instant, a partition healing.
//
// The node that sees it need not be either of the two, and cannot tell whether
// it is one of them, so it keeps serving: taking a healthy node down because a
// misconfigured one appeared loses availability for no correctness gain.
func (g *identityGuard) NotifyConflict(existing, other *memberlist.Node) {
	g.report(conflictLogMsg, existing.Name, existing.Address(), other.Address())
}

// note records where this node's id was found, for Register's message.
func (g *identityGuard) note(addr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.found == "" {
		g.found = addr
	}
}

// duplicate returns the address this node's id was found at, or "" if no
// exchange has found one.
func (g *identityGuard) duplicate() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.found
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
