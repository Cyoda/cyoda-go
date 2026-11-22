package dispatch_test

import (
	"testing"

	"github.com/cyoda/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda/cyoda-go/internal/contract"
)

func TestRandomSelector_SingleCandidate(t *testing.T) {
	s := dispatch.NewRandomSelector()
	candidates := []contract.NodeInfo{{NodeID: "node-1", Addr: "localhost:8123", Alive: true}}
	selected, err := s.Select(candidates)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selected.NodeID != "node-1" {
		t.Errorf("NodeID = %q, want %q", selected.NodeID, "node-1")
	}
}

func TestRandomSelector_MultipleCandidates(t *testing.T) {
	s := dispatch.NewRandomSelector()
	candidates := []contract.NodeInfo{
		{NodeID: "node-1", Addr: "localhost:8123", Alive: true},
		{NodeID: "node-2", Addr: "localhost:8124", Alive: true},
		{NodeID: "node-3", Addr: "localhost:8125", Alive: true},
	}
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		selected, err := s.Select(candidates)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		seen[selected.NodeID] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected at least 2 different nodes in 100 picks, got %d", len(seen))
	}
}

func TestRandomSelector_EmptyCandidates(t *testing.T) {
	s := dispatch.NewRandomSelector()
	_, err := s.Select(nil)
	if err == nil {
		t.Fatal("expected error for empty candidates")
	}
}
