package multinode

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// stubNoComputeClients is a MultiNodeFixture without ComputeClientCapable.
type stubNoComputeClients struct{}

func (stubNoComputeClients) BaseURLs() []string                     { return []string{"http://a", "http://b"} }
func (stubNoComputeClients) NodeCount() int                         { return 2 }
func (stubNoComputeClients) NewTenant(*testing.T) parity.Tenant     { return parity.Tenant{ID: "t"} }
func (stubNoComputeClients) ComputeTenant(*testing.T) parity.Tenant { return parity.Tenant{ID: "t"} }

var _ MultiNodeFixture = stubNoComputeClients{}

func TestComputeClientScenarioSkipsWithoutCapability(t *testing.T) {
	skipped := t.Run("ComputeClient_AttachToNode", func(t *testing.T) {
		RunComputeClient_AttachToNode(t, stubNoComputeClients{})
		t.Fatal("scenario did not skip on a fixture without ComputeClientCapable")
	})
	if !skipped {
		t.Error("ComputeClient_AttachToNode must t.Skip when the fixture cannot start compute clients")
	}
}

func TestComputeClientScenarioRegistered(t *testing.T) {
	for _, nt := range AllTests() {
		if nt.Name == "ComputeClient_AttachToNode" {
			return
		}
	}
	t.Error("ComputeClient_AttachToNode is not in the multinode registry")
}
