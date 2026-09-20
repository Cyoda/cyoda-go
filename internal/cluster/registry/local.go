package registry

import (
	"context"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// Local is a single-node NodeRegistry implementation. It always reports
// exactly one node — itself — as alive. It satisfies the contract.NodeRegistry
// interface and is the default implementation when cluster mode is disabled.
type Local struct {
	nodeID  string
	addr    string
	changed chan struct{} // never closed: a single pnode has no peers
}

// NewLocal constructs a Local registry representing a single node.
func NewLocal(nodeID, addr string) *Local {
	return &Local{nodeID: nodeID, addr: addr, changed: make(chan struct{})}
}

// Register is a no-op for a single-node registry: the node is already known
// at construction time.
func (l *Local) Register(_ context.Context, _ string, _ string) error {
	return nil
}

// Lookup returns the address and alive status for the given nodeID.
// For any nodeID other than the local node, alive is false and addr is empty.
func (l *Local) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	if nodeID == l.nodeID {
		return l.addr, true, nil
	}
	return "", false, nil
}

// List returns a single-element slice containing this node.
func (l *Local) List(_ context.Context) ([]contract.NodeInfo, error) {
	return []contract.NodeInfo{{NodeID: l.nodeID, Addr: l.addr, Alive: true}}, nil
}

// Deregister is a no-op for a single-node registry.
func (l *Local) Deregister(_ context.Context, _ string) error {
	return nil
}

// Changed returns a channel that is never closed: the view of a single pnode
// does not change.
func (l *Local) Changed() <-chan struct{} {
	return l.changed
}
