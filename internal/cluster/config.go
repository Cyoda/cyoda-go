package cluster

import "time"

type Config struct {
	Enabled  bool
	NodeID   string
	NodeAddr string
	// GRPCNodeAddr is this node's advertised gRPC endpoint (host:port, no scheme).
	// When set it is gossiped to peers; when empty peers derive it from NodeAddr's
	// host plus their own configured gRPC port.
	GRPCNodeAddr    string
	GossipAddr      string
	SeedNodes       []string
	StabilityWindow time.Duration
	ProxyTimeout    time.Duration
	HMACSecret      []byte
	// DispatchWaitTimeout is the patience: how long one callout waits, in
	// total, for a compute member to exist. It applies on a single node as in
	// a cluster. Zero disables waiting.
	DispatchWaitTimeout time.Duration
	// DispatchConnectTimeout bounds opening the connection for a hand-over to
	// another node. A peer that cannot be connected to costs no try.
	DispatchConnectTimeout time.Duration
	// DispatchForwardTimeout is the whole-request timeout of the scheduler's
	// node-to-node RPC client. It does not govern callout hand-overs.
	DispatchForwardTimeout time.Duration
	// DispatchAllowLoopback opts the inter-node dispatch HTTP forwarder out of
	// its loopback-address SSRF guard so multi-node tests can run every node on
	// 127.0.0.1 and still forward processor/criteria dispatch between them.
	// Sourced from CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING; defaults false.
	// Never enable in production — it re-opens the SSRF pivot the guard closes.
	DispatchAllowLoopback bool
}
