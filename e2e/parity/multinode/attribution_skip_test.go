package multinode

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// stubNoAttrFixture implements MultiNodeFixture but deliberately does NOT
// implement AttributionCapable — modelling a cluster-capable backend that has
// not yet wired cross-node attribution. The attribution scenarios must t.Skip
// (PENDING) against it, never run to completion or fail.
type stubNoAttrFixture struct{}

func (stubNoAttrFixture) BaseURLs() []string                 { return []string{"http://a", "http://b", "http://c"} }
func (stubNoAttrFixture) NodeCount() int                     { return 3 }
func (stubNoAttrFixture) NewTenant(*testing.T) parity.Tenant { return parity.Tenant{} }
func (stubNoAttrFixture) ComputeTenant(*testing.T) parity.Tenant {
	return parity.Tenant{}
}

// Compile-time proof the stub is a MultiNodeFixture but NOT AttributionCapable.
var _ MultiNodeFixture = stubNoAttrFixture{}

// TestAttributionScenariosSkipWithoutCapability asserts every attribution
// scenario reports an explicit PENDING SKIP — rather than running or failing —
// when the fixture does not implement AttributionCapable. This is the guarantee
// the out-of-tree commercial backend relies on: it consumes the shared registry
// and must see these as visible-but-pending, not as hard failures, until it
// wires ComputeUser/NodeLogs.
func TestAttributionScenariosSkipWithoutCapability(t *testing.T) {
	scenarios := []struct {
		name string
		fn   func(*testing.T, MultiNodeFixture)
	}{
		{"Attribution_ProxiedJoinCascade", RunAttribution_ProxiedJoinCascade},
		{"Attribution_ScheduledFire", RunAttribution_ScheduledFire},
		{"Attribution_CalloutAuthType", RunAttribution_CalloutAuthType},
	}
	for _, sc := range scenarios {
		sc := sc
		// A scenario that correctly skips marks the subtest skipped (t.Run ->
		// true). A scenario that runs past the capability gate hits the guard
		// Fatalf below, failing the subtest (t.Run -> false). Skip is the only
		// non-failing outcome, so t.Run() == true iff the scenario skipped.
		skipped := t.Run(sc.name, func(t *testing.T) {
			sc.fn(t, stubNoAttrFixture{})
			t.Fatalf("scenario %s did not skip despite fixture lacking AttributionCapable", sc.name)
		})
		if !skipped {
			t.Errorf("scenario %s must t.Skip when the fixture is not AttributionCapable", sc.name)
		}
	}
}

// TestAttributionScenariosRegistered asserts the three attribution scenarios are
// present in the shared registry (so every cluster-capable backend that iterates
// AllTests picks them up), guarding against an init() regression.
func TestAttributionScenariosRegistered(t *testing.T) {
	want := map[string]bool{
		"Attribution_ProxiedJoinCascade": false,
		"Attribution_ScheduledFire":      false,
		"Attribution_CalloutAuthType":    false,
	}
	for _, nt := range AllTests() {
		if _, ok := want[nt.Name]; ok {
			want[nt.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("attribution scenario %q not registered in the shared multinode registry", name)
		}
	}
}
