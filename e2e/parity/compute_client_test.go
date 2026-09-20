package parity

import "testing"

// stubNoComputeClients is a BackendFixture WITHOUT the compute-client
// capability — the shape of an out-of-tree backend that has not wired it yet.
type stubNoComputeClients struct{}

func (stubNoComputeClients) BaseURL() string                 { return "http://127.0.0.1:1" }
func (stubNoComputeClients) GRPCEndpoint() string            { return "127.0.0.1:1" }
func (stubNoComputeClients) NewTenant(*testing.T) Tenant     { return Tenant{ID: "t"} }
func (stubNoComputeClients) ComputeTenant(*testing.T) Tenant { return Tenant{ID: "t"} }

var _ BackendFixture = stubNoComputeClients{}

// TestComputeClientScenariosSkipWithoutCapability: a fixture that cannot start
// compute clients sees these scenarios as skipped, never failed, and before
// they touch the server.
func TestComputeClientScenariosSkipWithoutCapability(t *testing.T) {
	scenarios := map[string]func(*testing.T, BackendFixture){
		"ComputeClientJoinServeLeave": RunComputeClientJoinServeLeave,
		"ComputeClientBehaviours":     RunComputeClientBehaviours,
	}
	for name, fn := range scenarios {
		skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("scenario %s did not skip on a fixture without the capability", name)
		})
		if !skipped {
			t.Errorf("scenario %s must t.Skip when the fixture cannot start compute clients", name)
		}
	}
}

func TestComputeClientScenariosRegistered(t *testing.T) {
	want := map[string]bool{"ComputeClientJoinServeLeave": false, "ComputeClientBehaviours": false}
	for _, nt := range allTests {
		if _, ok := want[nt.Name]; ok {
			want[nt.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("scenario %s is not in the registry", name)
		}
	}
}
