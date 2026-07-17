package scheduler

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestLowestLiveNodeID(t *testing.T) {
	c := LowestLiveNodeID{}
	m := []contract.NodeInfo{{NodeID: "n3"}, {NodeID: "n1"}, {NodeID: "n2"}}
	if !c.IsCoordinator(m, "n1") {
		t.Error("n1 should be coordinator")
	}
	if c.IsCoordinator(m, "n2") {
		t.Error("n2 should not")
	}
	if !c.IsCoordinator(nil, "n1") {
		t.Error("empty membership → self is coordinator")
	}
}

func TestLowestLiveNodeID_OnlySelf(t *testing.T) {
	c := LowestLiveNodeID{}
	m := []contract.NodeInfo{{NodeID: "n1"}}
	if !c.IsCoordinator(m, "n1") {
		t.Error("membership containing only self → self is coordinator")
	}
}
