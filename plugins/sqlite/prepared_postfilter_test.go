package sqlite

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPlanQuery_PreparedPostFilterMatchesNilness pins the invariant the whole
// representation choice rests on: preparedPostFilter is non-nil EXACTLY when
// postFilter is non-nil.
//
// Absence must stay a nil pointer. A zero spi.PreparedFilter means match-all,
// not "no filter", so collapsing the two would silently cost LIMIT pushdown,
// disable native GROUP BY, and arm the scan budget on every query — with every
// returned result still correct, so nothing would fail loudly.
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
			plan := planQuery(tc.filter)
			if (plan.postFilter == nil) != (plan.preparedPostFilter == nil) {
				t.Fatalf("nil-ness diverged: postFilter==nil is %v, preparedPostFilter==nil is %v",
					plan.postFilter == nil, plan.preparedPostFilter == nil)
			}
			if plan.postFilter == nil {
				return
			}
			// And the prepared one must agree with the residual it stands for.
			data := []byte(`{"name":"Alice","a":null,"b":"x"}`)
			want := spi.Prepare(*plan.postFilter).Match(data, spi.EntityMeta{})
			if got := plan.preparedPostFilter.Match(data, spi.EntityMeta{}); got != want {
				t.Errorf("preparedPostFilter.Match = %v, want %v (the residual it stands for)", got, want)
			}
		})
	}
}

// TestSearch_MatchAllStillPushesLimitAndSkipsScanBudget pins the three
// consequences of postFilter absence, for BOTH spellings of match-all: the
// zero Filter{} and the explicit empty AND that ConditionToFilter emits for a
// nil condition. The two took different branches historically and must not
// drift apart again.
func TestSearch_MatchAllStillPushesLimitAndSkipsScanBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter spi.Filter
	}{
		{"zero filter", spi.Filter{}},
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var plan sqlPlan
			if tc.filter.Op != "" {
				plan = planQuery(tc.filter)
			}
			if plan.postFilter != nil {
				t.Fatalf("postFilter = %+v, want nil: a match-all query has nothing to post-filter, "+
					"and a non-nil residual costs LIMIT pushdown, disables native GROUP BY, "+
					"and arms the scan budget", plan.postFilter)
			}
			if plan.preparedPostFilter != nil {
				t.Fatalf("preparedPostFilter = %+v, want nil", plan.preparedPostFilter)
			}
		})
	}
}
