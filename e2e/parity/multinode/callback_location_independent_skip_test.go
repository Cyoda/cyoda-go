package multinode

import "testing"

// stubLIFixture is a cluster fixture that says whether its backend is
// location-independent.
type stubLIFixture struct {
	stubNoAttrFixture
	li bool
}

func (f stubLIFixture) LocationIndependent() bool { return f.li }

// TestLocationIndependentScenarioSkips: the scenario skips with "backend is not
// location-independent" on a fixture without the capability, and on one whose
// backend reports false.
func TestLocationIndependentScenarioSkips(t *testing.T) {
	for name, fixture := range map[string]MultiNodeFixture{
		"no capability":              stubNoAttrFixture{},
		"capability reporting false": stubLIFixture{li: false},
	} {
		skipped := t.Run(name, func(t *testing.T) {
			RunCallback_LocationIndependent_ServedOnNonOwner(t, fixture)
			t.Fatal("the scenario did not skip")
		})
		if !skipped {
			t.Errorf("%s: the scenario must skip", name)
		}
	}
}

// TestLocationIndependentScenarioRegistered: every cluster-capable backend
// that iterates AllTests picks the scenario up.
func TestLocationIndependentScenarioRegistered(t *testing.T) {
	for _, nt := range AllTests() {
		if nt.Name == "Callback_LocationIndependent_ServedOnNonOwner" {
			return
		}
	}
	t.Fatal("Callback_LocationIndependent_ServedOnNonOwner is not registered")
}
