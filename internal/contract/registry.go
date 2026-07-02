package contract

import "context"

type NodeInfo struct {
	NodeID   string
	Addr     string
	GRPCAddr string              // optional: explicit gRPC endpoint (host:port); empty means derive from Addr
	Alive    bool
	Tags     map[string][]string // tenantID → compute member tags
}

type NodeRegistry interface {
	Register(ctx context.Context, nodeID string, addr string) error
	Lookup(ctx context.Context, nodeID string) (addr string, alive bool, err error)
	List(ctx context.Context) ([]NodeInfo, error)
	Deregister(ctx context.Context, nodeID string) error
}
