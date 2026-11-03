package app

import (
	"context"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	clusterdispatch "github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

// clockControlWarning is logged at startup on a node with clock control on.
const clockControlWarning = "platform clock control is enabled: test feature, not for production; the clock is instance-wide and affects every tenant"

// asPlatform returns ctx carrying the platform clock's own principal in the
// PLATFORM tenant: ConsistencyTime requires a tenant in the context.
func asPlatform(ctx context.Context) context.Context {
	return spi.WithUserContext(ctx, &spi.UserContext{
		UserID:   "platform-clock",
		UserName: "Platform clock",
		Kind:     spi.PrincipalSystem,
		Tenant:   spi.Tenant{ID: auth.PlatformTenantID, Name: "Platform"},
	})
}

// platformConsistencyTime returns the store's consistency time read as the
// PLATFORM tenant: the read the clock service checks a set-back against.
func platformConsistencyTime(txMgr spi.TransactionManager) func(ctx context.Context) (time.Time, error) {
	return func(ctx context.Context) (time.Time, error) {
		return txMgr.ConsistencyTime(asPlatform(ctx))
	}
}

// platformLatestRecordedTime returns the store's latest recorded time, read
// without moving the floor.
func platformLatestRecordedTime(txMgr spi.TransactionManager) func(ctx context.Context) (time.Time, error) {
	return func(ctx context.Context) (time.Time, error) {
		return txMgr.LatestRecordedTime(asPlatform(ctx))
	}
}

// checkClockAtStartup decides the start of a node with clock control on. A
// setting taken from a peer is the cluster's clock, and the node runs on it.
// With none, the node starts on real time, and a store whose latest recorded
// time is later than real time fails the start (platformclock.StartupCheck).
// latestRecorded must not move the store's floor: a floor raised to real now
// would refuse the operator's first setting in the past.
func checkClockAtStartup(ctx context.Context, c *platformclock.Test,
	latestRecorded func(ctx context.Context) (time.Time, error), now time.Time) error {
	if c.Setting().Version != 0 {
		return nil
	}
	return platformclock.StartupCheck(ctx, latestRecorded, now)
}

// clockPeers returns the nodes a clock change is pushed to: every member of
// the gossip directory, read afresh for each confirmation round. The list
// includes this node; the round skips it.
func clockPeers(g *registry.Gossip) func() []platformclock.Peer {
	return func() []platformclock.Peer {
		members := g.Members()
		peers := make([]platformclock.Peer, len(members))
		for i, m := range members {
			peers[i] = platformclock.Peer{NodeID: m.NodeID, Addr: m.Addr}
		}
		return peers
	}
}

// clockPush pushes a clock setting to one peer over the sealed peer channel.
func clockPush(f *clusterdispatch.HTTPForwarder) platformclock.Push {
	return func(ctx context.Context, peer platformclock.Peer, s platformclock.Setting) (platformclock.Setting, error) {
		return f.PushClock(ctx, peer.NodeID, peer.Addr, s)
	}
}
