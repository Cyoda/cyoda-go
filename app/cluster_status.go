package app

import (
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/clusterstatus"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

// gossipDirectory is the status read's directory in cluster mode: every member
// of the gossip directory, this node included, with the state its metadata
// names, and the registry's change signal.
type gossipDirectory struct{ g *registry.Gossip }

func (d gossipDirectory) Members() []clusterstatus.Member {
	members := d.g.Members()
	out := make([]clusterstatus.Member, len(members))
	for i, m := range members {
		out[i] = clusterstatus.Member{ID: m.NodeID, Addr: m.Addr, State: m.State}
	}
	return out
}

func (d gossipDirectory) Changed() <-chan struct{} { return d.g.Changed() }

// statusClock returns this node's clock setting without its times: the test
// clock's version and writer with clock control on (c non-nil), and the zero
// value with it off.
func statusClock(c *platformclock.Test) func() clusterstatus.Clock {
	if c == nil {
		return func() clusterstatus.Clock { return clusterstatus.Clock{} }
	}
	return func() clusterstatus.Clock {
		s := c.Setting()
		return clusterstatus.Clock{Controllable: true, Version: s.Version, Writer: s.Writer}
	}
}
