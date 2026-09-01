package postgres

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPlanFor_MatchAllLeavesNoResidual pins the match-all guard on the
// production helper every search entry point plans through. Mirrors sqlite's
// test of the same name — the two planners are deliberate mirrors, so the guard
// and its coverage must mirror too.
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
// This is the black-box-unprovable half called out in prepared_postfilter_test.go:
// on Search's LIMIT-pushdown branch the pushdown and residual paths return the
// identical error, so the guard cannot be observed through Search at all. Naming
// it planFor and asserting it directly is what closes that gap.
func TestPlanFor_MatchAllLeavesNoResidual(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter spi.Filter
	}{
		{"zero filter", spi.Filter{}},
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planFor(tc.filter)
			if err != nil {
				t.Fatalf("planFor: %v", err)
			}
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
	plan, err := planFor(spi.Filter{Op: spi.FilterOr})
	if err != nil {
		t.Fatalf("planFor: %v", err)
	}
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
	got, err := planFor(f)
	if err != nil {
		t.Fatalf("planFor: %v", err)
	}
	want, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if got.where != want.where {
		t.Errorf("where = %q, want %q", got.where, want.where)
	}
	if (got.postFilter == nil) != (want.postFilter == nil) {
		t.Errorf("postFilter nil-ness = %v, want %v", got.postFilter == nil, want.postFilter == nil)
	}
}

// TestPlanFor_MixedFilterPushesTheEqLeaf mirrors sqlite's test of the same name.
// A mixed AND — a pushable eq(city) beside a non-pushable regex — must push the
// eq leaf into SQL and keep only the regex work in Go. If the eq stopped being
// pushed, `where` would come back empty and this filter shape would scan the
// whole model.
//
// That is invisible from Search's own results: the soundness gate installs the
// FULL original filter as the residual whenever a plan is not provably exact, so
// a full scan re-checked in Go returns exactly the same rows as a narrowed one.
func TestPlanFor_MixedFilterPushesTheEqLeaf(t *testing.T) {
	mixed := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		{Op: spi.FilterEq, Path: "city", Source: spi.SourceData,
			Value: "Berlin", Declared: []spi.DataType{spi.String}},
		{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: ".*"},
	}}

	plan, err := planFor(mixed)
	if err != nil {
		t.Fatalf("planFor: %v", err)
	}
	if plan.where == "" {
		t.Fatal("where is empty: the pushable eq(city) leaf was not pushed, so this " +
			"filter shape now full-scans the model")
	}
	if len(plan.args) == 0 {
		t.Errorf("args = %v, want the bound operand alongside the WHERE fragment", plan.args)
	}
	// The regex half is not expressible in SQL, so the plan is not exact and the
	// kernel must re-check every candidate the narrowing SQL returns.
	if plan.postFilter == nil {
		t.Error("postFilter = nil: a non-pushable regex leaf must leave a residual")
	}
}
