package parity

import "testing"

var calloutScenarios = map[string]func(*testing.T, BackendFixture){
	"CalloutNoAnswerNotIdempotentStops": RunCalloutNoAnswerNotIdempotentStops,
	"CalloutCriterionFailsOver":         RunCalloutCriterionFailsOver,
	"CalloutFunctionFailsOver":          RunCalloutFunctionFailsOver,
	"CalloutMemberFailedStops":          RunCalloutMemberFailedStops,
	"CalloutRetryPolicyNoneOneTry":      RunCalloutRetryPolicyNoneOneTry,
	"CalloutEveryTryUsed":               RunCalloutEveryTryUsed,
}

// TestCalloutScenariosSkipWithoutCapability: on a backend whose fixture cannot
// start compute clients these scenarios skip, before they touch the server.
func TestCalloutScenariosSkipWithoutCapability(t *testing.T) {
	for name, fn := range calloutScenarios {
		skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("scenario %s did not skip on a fixture without the capability", name)
		})
		if !skipped {
			t.Errorf("scenario %s must t.Skip when the fixture cannot start compute clients", name)
		}
	}
}

func TestCalloutScenariosRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, nt := range allTests {
		registered[nt.Name] = true
	}
	for name := range calloutScenarios {
		if !registered[name] {
			t.Errorf("scenario %s is not in the registry", name)
		}
	}
}
