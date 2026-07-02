package cluster

import "time"

type Config struct {
	Enabled                bool
	NodeID                 string
	NodeAddr               string
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
