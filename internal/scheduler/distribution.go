package scheduler

import (
	"sort"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// DistributionStrategy picks which cluster member a due ScheduledTask should
// be dispatched to. Implementations operate purely on the passed-in
// members/selfID/task — no store or registry calls of their own.
type DistributionStrategy interface {
	Pick(members []contract.NodeInfo, selfID string, task spi.ScheduledTask) string
}

// RoundRobin is the default DistributionStrategy: it rotates over the
// member IDs in deterministic (sorted) order, independent of gossip
// ordering, so behavior is stable and testable. The cursor is shared
// coordinator-side state guarded by a mutex.
type RoundRobin struct {
	mu     sync.Mutex
	cursor uint64
}

// NewRoundRobin returns a fresh RoundRobin with its cursor at zero.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Pick sorts member NodeIDs and returns the next one in rotation. With no
// members, it falls back to selfID (single-node / not-yet-gossiped case).
func (d *RoundRobin) Pick(members []contract.NodeInfo, selfID string, _ spi.ScheduledTask) string {
	if len(members) == 0 {
		return selfID
	}

	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.NodeID
	}
	sort.Strings(ids)

	d.mu.Lock()
	defer d.mu.Unlock()
	idx := d.cursor % uint64(len(ids))
	d.cursor++
	return ids[idx]
}

// Self is a DistributionStrategy that always dispatches locally.
type Self struct{}

// Pick always returns selfID, ignoring members and task.
func (Self) Pick(_ []contract.NodeInfo, selfID string, _ spi.ScheduledTask) string {
	return selfID
}
