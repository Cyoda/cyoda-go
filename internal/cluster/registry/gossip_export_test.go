package registry

import (
	"fmt"

	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

// LocalAddr exposes the address memberlist derived for this node, for tests
// only: a node bound on an ephemeral port must be looked up after the fact
// rather than assumed in advance, and this is how a test that seeds a second
// node off the first one learns what the OS gave it. Production code has no
// equivalent need — peers learn a node's address through the join exchange
// itself, not by asking it directly.
func (g *Gossip) LocalAddr() string {
	return g.list.LocalNode().Address()
}

// Crash stops this node the way a process kill does: the listener goes down
// without a leave message, so the peers keep the record of it — alive, at the
// address it had — until their own failure detector reaps it. That is the
// state a restarting node has to wait out, and a test cannot produce it with
// Deregister, which announces the leave. For tests only; nothing in production
// ends a node this way on purpose. It shares Deregister's once, so a cleanup
// that deregisters afterwards does nothing.
func (g *Gossip) Crash() {
	g.deregisterOnce.Do(func() {
		close(g.stop)
		<-g.done
		<-g.advertised
		g.metrics.close()
		if err := g.list.Shutdown(); err != nil {
			g.deregisterErr = fmt.Errorf("shutdown memberlist: %w", err)
		}
	})
}

// LocalStateForTest returns what memberlist would send in a push/pull
// exchange; join says whether the exchange is a join.
func (g *Gossip) LocalStateForTest(join bool) []byte { return g.delegate.LocalState(join) }

// MergeRemoteStateForTest delivers a push/pull payload as memberlist would.
func (g *Gossip) MergeRemoteStateForTest(buf []byte, join bool) {
	g.delegate.MergeRemoteState(buf, join)
}

// MemberStateForTest returns the lifecycle state this node's directory holds
// for the member called name; false when the member is not alive here or its
// metadata names no valid state.
func (g *Gossip) MemberStateForTest(name string) (lifecycle.State, bool) {
	m, ok := g.dir.get(name)
	if !ok {
		return "", false
	}
	return memberState(m.Meta)
}
