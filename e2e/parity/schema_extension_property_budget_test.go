package parity

import (
	"fmt"
	"strings"
	"testing"
)

// PropertyIterationBudget is the per-backend iteration ceiling for the
// SchemaExtensionByteIdentityProperty entry: propertyNumSeeds (50) ×
// extensionsPerSeed (8) = 400, matching the per-backend CI share Spec §7.3
// sets (120 s total CI ceiling across 3 backends; ~10 s observed per
// backend at this iteration count).
//
// This is a frozen ceiling, not a value derived from propertyNumSeeds and
// extensionsPerSeed — bumping either of those constants must raise this
// one too, as its own deliberate CI-budget decision. See
// TestPropertyIterationBudget_MatchesCurrentConstants, which fails the
// moment the two drift apart.
const PropertyIterationBudget = 400

// checkPropertyIterationBudget reports whether seeds*extensionsPerSeed
// exceeds budget. It is pure and deterministic — no wall clock, no
// backend — so a seed-count or extensionsPerSeed regression fails the
// same way on every run, including under machine load, instead of
// depending on how fast the current run happened to go.
func checkPropertyIterationBudget(seeds, extensionsPerSeed, budget int) error {
	iterations := seeds * extensionsPerSeed
	if iterations > budget {
		return fmt.Errorf(
			"SchemaExtensionByteIdentityProperty iteration count %d (seeds=%d x extensionsPerSeed=%d) "+
				"exceeds the per-backend CI budget %d; raising either constant is a deliberate CI-budget "+
				"decision — bump PropertyIterationBudget alongside it",
			iterations, seeds, extensionsPerSeed, budget)
	}
	return nil
}

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
