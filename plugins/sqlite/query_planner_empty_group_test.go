package sqlite

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The SPI filter contract (spi.MatchFilter): an explicit empty AND is the AND
// identity (true, matches everything); an explicit empty OR is the OR identity
// (false, matches nothing). SQL cannot express either shape — joining zero
// fragments yields no predicate at all (i.e. TRUE) at the top level and a
// malformed "()" when nested — so the planner must keep empty groups out of
// the pushed SQL and let the kernel apply the identity.

// An explicit empty OR must fall to the residual so the kernel applies the OR
// identity (false). Pushing it emits no WHERE clause and no post-filter — a
// query that must match nothing silently matching everything.
func TestPlanQuery_EmptyOR(t *testing.T) {
	f := spi.Filter{Op: spi.FilterOr}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty for an empty OR, got %s", plan.where)
	}
	if plan.postFilter == nil {
		t.Fatal("postFilter should be the empty-OR residual (kernel applies the OR identity: match nothing); nil means match-all")
	}
	if plan.postFilter.Op != spi.FilterOr {
		t.Errorf("postFilter.Op = %s, want or", plan.postFilter.Op)
	}
}

// An explicit empty AND is the AND identity (true): match-all is correctly
// expressed as no WHERE clause and no residual. Pins existing behavior.
func TestPlanQuery_EmptyAND(t *testing.T) {
	f := spi.Filter{Op: spi.FilterAnd}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty for an empty AND, got %s", plan.where)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil for an empty AND (identity: match all), got %+v", plan.postFilter)
	}
}

// An empty OR nested under AND must not reach the pushed SQL (joinChildren
// over zero children nests as malformed "()"); it falls to the residual,
// forcing the full-filter kernel re-check. The IsNull sibling is chosen
// because it is EXACT — without the fix the plan is certified exact and no
// re-check happens at all.
func TestPlanQuery_EmptyORNestedInAND(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterIsNull, Path: "status", Source: spi.SourceData},
			{Op: spi.FilterOr},
		},
	}
	plan := planQuery(f)
	if strings.Contains(plan.where, "()") {
		t.Errorf("where contains malformed empty group: %s", plan.where)
	}
	if plan.postFilter == nil {
		t.Fatal("postFilter should be the full AND filter (empty-OR child makes the whole conjunction false; only the kernel applies that)")
	}
	if plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter.Op = %s, want and (full filter)", plan.postFilter.Op)
	}
}

// An empty AND nested under OR is the identity true — the whole OR matches
// everything. SQL cannot express the empty group ("()" is malformed), so the
// entire OR goes residual (conservative OR dissection) and the kernel
// evaluates it.
func TestPlanQuery_EmptyANDNestedInOR(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterIsNull, Path: "status", Source: spi.SourceData},
			{Op: spi.FilterAnd},
		},
	}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty (whole OR residual), got %s", plan.where)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterOr {
		t.Fatalf("postFilter should be the whole OR residual, got %+v", plan.postFilter)
	}
}

// allPushedExact must never certify an empty group as EXACT: exactness of a
// group is a claim about its leaves, and an empty group has none — its
// identity semantics are the kernel's to apply, so a vacuous true here would
// skip the kernel re-check.
func TestAllPushedExact_EmptyGroupNotExact(t *testing.T) {
	if allPushedExact(spi.Filter{Op: spi.FilterOr}) {
		t.Error("allPushedExact(empty OR) = true, want false")
	}
	if allPushedExact(spi.Filter{Op: spi.FilterAnd}) {
		t.Error("allPushedExact(empty AND) = true, want false")
	}
}
