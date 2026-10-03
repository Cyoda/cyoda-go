package parity

import (
	"fmt"
	"testing"
	"time"
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

// RunSchemaExtensionPropertyBudget re-runs the property entry against the
// given fixture and asserts its iteration count — not its wall clock —
// stays within the per-backend CI budget.
//
// A wall-clock ceiling was tried first and is a flake by construction in
// this suite: the parity packages run in parallel, so the same work that
// normally takes ~10 s took 74 s under machine load and failed, then
// passed on an unchanged re-run. checkPropertyIterationBudget replaces
// that with a deterministic guard on the two constants that actually
// drive the entry's cost (propertyNumSeeds, extensionsPerSeed): a
// regression that bumps either one fails every run, load or no load.
//
// What this does NOT catch: a per-iteration cost increase at a fixed
// iteration count — e.g. an engine-side slowdown that makes each
// CreateEntity or ExportModel call take longer without changing how many
// calls are made. No deterministic proxy for that was found (the
// generated payload shape (gentreeDepth/gentreeWidth) is a weak proxy at
// best, since network round-trip latency, not local payload generation,
// dominates the observed ~10 s). That class of regression is no longer
// asserted here; the wall clock is still logged below for a human
// reading CI output to notice, but a slowdown confined to it will not
// fail the build.
func RunSchemaExtensionPropertyBudget(t *testing.T, fixture BackendFixture) {
	if testing.Short() {
		t.Skip("property-budget check runs only in full mode")
	}

	if err := checkPropertyIterationBudget(propertyNumSeeds, extensionsPerSeed, PropertyIterationBudget); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	RunSchemaExtensionByteIdentityProperty(t, fixture)
	elapsed := time.Since(start)
	t.Logf("SchemaExtensionByteIdentityProperty wall clock: %v (logged only, not asserted — see iteration-count budget above)", elapsed)
}
