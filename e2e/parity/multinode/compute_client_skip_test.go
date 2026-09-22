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

// TestCalloutHandOverScenariosSkipAndAreRegistered covers the hand-over and
// fencing scenarios the same way: each must be in the registry, and each must
// skip — not fail — on a cluster fixture that cannot start compute clients.
func TestCalloutHandOverScenariosSkipAndAreRegistered(t *testing.T) {
	scenarios := map[string]func(*testing.T, MultiNodeFixture){
		"Callout_HandOverSucceeds":                 RunCallout_HandOverSucceeds,
		"Callout_HandOverTwoTriesInOneExchange":    RunCallout_HandOverTwoTriesInOneExchange,
		"Callout_HandOverCarriesMessageAndVerdict": RunCallout_HandOverCarriesMessageAndVerdict,
		"Callout_TwoTenantsShareATag":              RunCallout_TwoTenantsShareATag,
		"Callout_PassFromAnotherPnode":             RunCallout_PassFromAnotherPnode,
		"Callout_MinorAbsorbedAcrossHandOvers":     RunCallout_MinorAbsorbedAcrossHandOvers,
	}
	registered := map[string]bool{}
	for _, nt := range AllTests() {
		registered[nt.Name] = true
	}
	for name, fn := range scenarios {
		if !registered[name] {
			t.Errorf("%s is not in the multinode registry", name)
		}
		if skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("%s did not skip on a fixture without ComputeClientCapable", name)
		}); !skipped {
			t.Errorf("%s must t.Skip when the fixture cannot start compute clients", name)
		}
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
