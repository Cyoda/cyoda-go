package parity

import (
	"strings"
	"testing"
)

// These tests cover checkPropertyIterationBudget in isolation — no
// backend fixture, no wall clock — so the budget's own logic is exercised
// deterministically on every run, not only when the full property suite
// happens to execute.

func TestCheckPropertyIterationBudget_WithinBudget_NoError(t *testing.T) {
	if err := checkPropertyIterationBudget(10, 5, 100); err != nil {
		t.Errorf("10*5=50 within budget 100, want nil error, got: %v", err)
	}
}

func TestCheckPropertyIterationBudget_ExactlyAtBudget_NoError(t *testing.T) {
	if err := checkPropertyIterationBudget(10, 10, 100); err != nil {
		t.Errorf("10*10=100 == budget 100, want nil error, got: %v", err)
	}
}

func TestCheckPropertyIterationBudget_ExceedsBudget_Errors(t *testing.T) {
	err := checkPropertyIterationBudget(10, 11, 100)
	if err == nil {
		t.Fatal("10*11=110 > budget 100, want an error, got nil")
	}
	for _, want := range []string{"110", "100", "seeds=10", "extensionsPerSeed=11"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q for a reproducible diagnosis, got: %v", want, err)
		}
	}
}

// TestPropertyIterationBudget_MatchesCurrentConstants locks the budget to
// the property entry's current seed/extension product with no slack. The
// budget is a deliberately frozen ceiling, not a value that tracks the
// source constants automatically — if this test fails after bumping
// propertyNumSeeds or extensionsPerSeed, PropertyIterationBudget must be
// raised alongside it as its own, separately-reviewed decision.
func TestPropertyIterationBudget_MatchesCurrentConstants(t *testing.T) {
	current := propertyNumSeeds * extensionsPerSeed
	if current != PropertyIterationBudget {
		t.Errorf("propertyNumSeeds*extensionsPerSeed = %d, PropertyIterationBudget = %d; "+
			"these must match exactly — raise the budget deliberately, don't let it drift",
			current, PropertyIterationBudget)
	}
}
