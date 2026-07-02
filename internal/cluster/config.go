package cluster

import "time"

type Config struct {
	Enabled                bool
	NodeID                 string
	NodeAddr               string
	// GRPCNodeAddr is this node's advertised gRPC endpoint (host:port, no scheme).
	// When set it is gossiped to peers; when empty peers derive it from NodeAddr's
	// host plus their own configured gRPC port.
	GRPCNodeAddr           string
	GossipAddr             string
	SeedNodes              []string
	StabilityWindow        time.Duration
	TxTTL                  time.Duration
	TxReapInterval         time.Duration
	ProxyTimeout           time.Duration
	OutcomeTTL             time.Duration
	HMACSecret             []byte
	DispatchWaitTimeout    time.Duration
	DispatchForwardTimeout time.Duration
	TxTokenTTL             time.Duration
	// DispatchAllowLoopback opts the inter-node dispatch HTTP forwarder out of
	// its loopback-address SSRF guard so multi-node tests can run every node on
	// 127.0.0.1 and still forward processor/criteria dispatch between them.
	// Sourced from CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING; defaults false.
	// Never enable in production — it re-opens the SSRF pivot the guard closes.
	DispatchAllowLoopback bool
}
