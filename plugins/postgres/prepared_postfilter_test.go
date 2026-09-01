package postgres

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPlanQuery_PreparedPostFilterMatchesNilness pins the invariant the whole
// representation choice rests on: preparedPostFilter is non-nil EXACTLY when
// postFilter is non-nil.
//
// Absence must stay a nil pointer. A zero spi.PreparedFilter means match-all,
// not "no filter", so collapsing the two would silently cost LIMIT pushdown
// and disable native GROUP BY on every query — with every returned result
// still correct, so nothing would fail loudly.
func TestPlanQuery_PreparedPostFilterMatchesNilness(t *testing.T) {
	cases := []struct {
		name   string
		filter spi.Filter
	}{
		{"exact leaf, no residual", spi.Filter{
			Op: spi.FilterIsNull, Source: spi.SourceData, Path: "name"}},
		{"inexact leaf forces a full re-check", spi.Filter{
			Op: spi.FilterEq, Source: spi.SourceData, Path: "name",
			Value: "Alice", Declared: []spi.DataType{spi.String}}},
		{"unpushable leaf becomes a residual", spi.Filter{
			Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "name",
			Value: "A.*", Declared: []spi.DataType{spi.String}}},
		{"mixed AND", spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
			{Op: spi.FilterIsNull, Source: spi.SourceData, Path: "a"},
			{Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "b",
				Value: "x", Declared: []spi.DataType{spi.String}},
		}}},
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
		{"explicit empty OR", spi.Filter{Op: spi.FilterOr}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planQuery(tc.filter)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
			if (plan.postFilter == nil) != (plan.preparedPostFilter == nil) {
				t.Fatalf("nil-ness diverged: postFilter==nil is %v, preparedPostFilter==nil is %v",
					plan.postFilter == nil, plan.preparedPostFilter == nil)
			}
			if plan.postFilter == nil {
				return
			}
			// And the prepared one must agree with the residual it stands for.
			data := []byte(`{"name":"Alice","a":null,"b":"x"}`)
			wantPF, err := spi.Prepare(*plan.postFilter)
			if err != nil {
				t.Fatalf("spi.Prepare: %v", err)
			}
			want := wantPF.Match(data, spi.EntityMeta{})
			if got := plan.preparedPostFilter.Match(data, spi.EntityMeta{}); got != want {
				t.Errorf("preparedPostFilter.Match = %v, want %v (the residual it stands for)", got, want)
			}
		})
	}
}

// TestSearch_MatchAllLeavesNoResidual pins the precondition that opens the
// gates postFilter absence controls on postgres: LIMIT pushdown
// (searcher.go:211) and native GROUP BY (grouped_stats.go:223). A non-nil
// residual is what would cost LIMIT pushdown and disable native GROUP BY, so
// asserting absence here is asserting those gates stay open.
//
// This covers only the explicit empty AND that ConditionToFilter emits for a
// nil condition — planQuery is called directly with it below, so a broken
// dissect()/planQuery would actually fail this test.
//
// The other spelling of match-all, the zero Filter{}, is deliberately NOT
// exercised here: production never passes it to planQuery at all. The guard
// lives in planFor, which every search path plans through, and is asserted
// directly by TestPlanFor_MatchAllLeavesNoResidual (plan_for_test.go) —
// together with TestPlanFor_EmptyOrIsNotMatchAll, which pins the asymmetry that
// makes the guard non-obvious: an empty AND is match-all, an empty OR is the
// false identity and must keep its residual.
//
// Naming the guard is what made it assertable. It used to be four inline
// copies, pinned only indirectly: GroupedAggregate through
// TestPostgresGroupedAggregate_PushesCountByState (grouped_stats_test.go), and
// Search's LIMIT-pushdown branch not at all — the pushdown and residual
// branches return the identical ErrSearchResultLimitExceeded (per
// TestPGSearcher_PushdownOverLimitFails / TestPGSearcher_ResidualOverLimitFails
// in searcher_test.go), so no black-box observation could tell them apart. That
// gap is now closed at the planner, not worked around with a trivially-true
// assertion here.
func TestSearch_MatchAllLeavesNoResidual(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter spi.Filter
	}{
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planQuery(tc.filter)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
			if plan.postFilter != nil {
				t.Fatalf("postFilter = %+v, want nil: a match-all query has nothing to post-filter, "+
					"and a non-nil residual costs LIMIT pushdown and disables native GROUP BY", plan.postFilter)
			}
			if plan.preparedPostFilter != nil {
				t.Fatalf("preparedPostFilter = %+v, want nil", plan.preparedPostFilter)
			}
		})
	}
}
