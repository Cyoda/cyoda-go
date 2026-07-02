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
}
