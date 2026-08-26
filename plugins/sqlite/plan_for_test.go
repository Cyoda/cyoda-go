package sqlite

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPlanFor_MatchAllLeavesNoResidual pins the match-all guard on the
// production helper every search entry point routes through.
//
// Both spellings of "match everything" — the zero spi.Filter{} and the explicit
// empty AND that ConditionToFilter emits for a nil condition — must plan to
// nothing: no WHERE, no residual. planQuery alone does NOT satisfy this for the
// zero Filter{}; it treats the empty Op as a non-pushable leaf and installs the
// zero filter as its own residual. Since a zero spi.PreparedFilter means
// match-all, every returned row would still be correct, so the damage is
// silent: a non-nil residual costs LIMIT pushdown and disables native GROUP BY
// on a query with nothing to post-filter.
//
// The guard used to live inline at each of the four call sites, pinned only
// through the residual-scan budget's externally observable failure. That budget
// is gone (search bounding is the caller's, not the backend's), so the guard is
// a named function here and asserted directly instead.
func TestPlanFor_MatchAllLeavesNoResidual(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter spi.Filter
	}{
		{"zero filter", spi.Filter{}},
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planFor(tc.filter)
			if plan.postFilter != nil {
				t.Errorf("postFilter = %+v, want nil: a match-all query has nothing to "+
					"post-filter, and a non-nil residual costs LIMIT pushdown and "+
					"disables native GROUP BY", plan.postFilter)
			}
			if plan.preparedPostFilter != nil {
				t.Errorf("preparedPostFilter = %+v, want nil", plan.preparedPostFilter)
			}
			if plan.where != "" {
				t.Errorf("where = %q, want empty", plan.where)
			}
		})
	}
}

// TestPlanFor_EmptyOrIsNotMatchAll guards the asymmetry the guard must not
// swallow. An explicit empty OR is the OR identity — false, matching NOTHING —
// not match-all, so it must keep its residual and let the kernel apply the
// identity. Only the empty Op and the empty AND (whose identity is true) are
// match-all. Widening planFor's short-circuit to any childless group would turn
// "match nothing" into "match everything".
func TestPlanFor_EmptyOrIsNotMatchAll(t *testing.T) {
	plan := planFor(spi.Filter{Op: spi.FilterOr})
	if plan.postFilter == nil {
		t.Fatal("postFilter = nil: an empty OR matches nothing and must stay a residual")
	}
	if plan.preparedPostFilter == nil {
		t.Fatal("preparedPostFilter = nil, want non-nil alongside postFilter")
	}
}

// TestPlanFor_DelegatesToPlanQuery: planFor short-circuits only match-all. A
// real filter must plan exactly as planQuery does, so the guard cannot quietly
// swallow a filter that should have been pushed down.
func TestPlanFor_DelegatesToPlanQuery(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterEq, Source: spi.SourceData, Path: "name",
		Value: "Alice", Declared: []spi.DataType{spi.String},
	}
	got, want := planFor(f), planQuery(f)
	if got.where != want.where {
		t.Errorf("where = %q, want %q", got.where, want.where)
	}
	if (got.postFilter == nil) != (want.postFilter == nil) {
		t.Errorf("postFilter nil-ness = %v, want %v", got.postFilter == nil, want.postFilter == nil)
	}
}
