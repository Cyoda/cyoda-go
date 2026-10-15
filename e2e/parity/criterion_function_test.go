package parity

import "testing"

var criterionFunctionScenarios = map[string]func(*testing.T, BackendFixture){
	"CriterionFunction_GroupStopRule":             RunCriterionFunction_GroupStopRule,
	"CriterionFunction_SelectionPreCheck":         RunCriterionFunction_SelectionPreCheck,
	"CriterionFunction_SelectionGroup":            RunCriterionFunction_SelectionGroup,
	"CriterionFunction_ManualRefusedReason":       RunCriterionFunction_ManualRefusedReason,
	"CriterionFunction_AuditKeepsPreCheckNumbers": RunCriterionFunction_AuditKeepsPreCheckNumbers,
}

// TestCriterionFunctionScenariosSkipWithoutCapability: on a backend whose
// fixture cannot start compute clients these scenarios skip, before they
// touch the server.
func TestCriterionFunctionScenariosSkipWithoutCapability(t *testing.T) {
	for name, fn := range criterionFunctionScenarios {
		skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("scenario %s did not skip on a fixture without the capability", name)
		})
		if !skipped {
			t.Errorf("scenario %s must t.Skip when the fixture cannot start compute clients", name)
		}
	}
}

func TestCriterionFunctionScenariosRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, nt := range allTests {
		registered[nt.Name] = true
	}
	for name := range criterionFunctionScenarios {
		if !registered[name] {
			t.Errorf("scenario %s is not in the registry", name)
		}
	}
}
