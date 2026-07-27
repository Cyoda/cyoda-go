package scheduler

import "github.com/cyoda-platform/cyoda-go/internal/contract"

// CoordinatorStrategy decides which cluster member is responsible for
// scanning for due ScheduledTasks on a given tick. Non-coordinators idle.
type CoordinatorStrategy interface {
	IsCoordinator(members []contract.NodeInfo, selfID string) bool
}

// LowestLiveNodeID is the default CoordinatorStrategy: the member with the
// lexicographically smallest NodeID is the coordinator. When membership is
// empty (or contains only self, e.g. single-node or not-yet-gossiped), self
// is the coordinator.
type LowestLiveNodeID struct{}

// IsCoordinator reports whether selfID is the minimum NodeID across members.
func (LowestLiveNodeID) IsCoordinator(members []contract.NodeInfo, selfID string) bool {
	if len(members) == 0 {
		return true
	}

	min := members[0].NodeID
	for _, m := range members[1:] {
		if m.NodeID < min {
			min = m.NodeID
		}
	}
	return selfID == min
}
