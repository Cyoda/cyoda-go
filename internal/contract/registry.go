package contract

import "context"

type NodeInfo struct {
	NodeID   string
	Addr     string
	GRPCAddr string // optional: explicit gRPC endpoint (host:port); empty means derive from Addr
	Alive    bool
	Tags     map[string][]string // tenantID → compute member tags
}

type NodeRegistry interface {
	Register(ctx context.Context, nodeID string, addr string) error
	Lookup(ctx context.Context, nodeID string) (addr string, alive bool, err error)
	List(ctx context.Context) ([]NodeInfo, error)
	Deregister(ctx context.Context, nodeID string) error
	// Changed returns a channel that is closed at the next change of the
	// cluster view: a peer's tag list arriving, a peer joining, a peer
	// leaving. Every change installs a new channel. A caller that waits for
	// the view to change takes the channel BEFORE it calls List, so a change
	// between the two is not missed.
	Changed() <-chan struct{}
}
