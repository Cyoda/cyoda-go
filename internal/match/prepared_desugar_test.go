package match_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// deepChainCondition builds a chain of depth nested AND-groups, each
// wrapping exactly one child: a leaf SimpleCondition at the bottom, a nested
// GroupCondition everywhere above it. Same shape as
// spi.deepChainCondition (condition_filter_desugar_test.go in the SPI
// module) — this is match.Prepare's sibling of the same defect:
// spi.DesugarCondition desugars cond's WHOLE tree in one call, but
// prepareGroup (prepared.go) called match's own `prepare` again for each
// child, which re-ran spi.DesugarCondition on that child's
// already-desugared subtree. For a depth-D chain that telescopes into
// D + (D-1) + ... + 1 = O(D²) group-node revisits instead of O(D).
func deepChainCondition(depth int) predicate.Condition {
	var c predicate.Condition = &predicate.SimpleCondition{
		JsonPath: "$.leaf", OperatorType: "EQUALS", Value: "x",
	}
	for i := 0; i < depth; i++ {
		c = &predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{c}}
	}
	return c
}

// TestPrepare_DesugarIsNotReappliedPerLevel is match.Prepare's counterpart to
// spi's TestConditionToFilter_DesugarIsNotReappliedPerLevel. See that test's
// doc for why testing.AllocsPerRun (deterministic, no CI/sandbox timing
// noise) and a depth-doubling ratio (linear ~2x, quadratic ~4x, 3x cutoff
// between them) are the right tool here rather than a wall-clock bound or a
// hard-coded absolute count.
func TestPrepare_DesugarIsNotReappliedPerLevel(t *testing.T) {
	const small, large = 40, 80

	condSmall := deepChainCondition(small)
	condLarge := deepChainCondition(large)

	allocsSmall := testing.AllocsPerRun(20, func() {
		if _, err := match.Prepare(condSmall, nil); err != nil {
			t.Fatalf("Prepare(depth=%d): %v", small, err)
		}
	})
	allocsLarge := testing.AllocsPerRun(20, func() {
		if _, err := match.Prepare(condLarge, nil); err != nil {
			t.Fatalf("Prepare(depth=%d): %v", large, err)
		}
	})

	if allocsSmall <= 0 {
		t.Fatalf("sanity: allocsSmall = %v, want > 0", allocsSmall)
	}
	ratio := allocsLarge / allocsSmall
	if ratio > 3 {
		t.Errorf("doubling chain depth (%d -> %d) multiplied allocations by %.2fx (allocsSmall=%v allocsLarge=%v); "+
			"want well under 4x (quadratic) — spi.DesugarCondition is being re-applied per ancestor level instead of once for the whole tree",
			small, large, ratio, allocsSmall, allocsLarge)
	}
}
