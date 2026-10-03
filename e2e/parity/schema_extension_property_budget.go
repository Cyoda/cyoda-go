package parity

import (
	"fmt"
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
