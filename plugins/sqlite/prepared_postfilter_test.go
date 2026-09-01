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
// not "no filter", so collapsing the two would silently cost LIMIT pushdown and
// disable native GROUP BY on every query — with every returned result still
// correct, so nothing would fail loudly.
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

// The match-all guard that keeps those gates open is asserted in
// plan_for_test.go, not here. It lives in planFor — the helper searchCommitted,
// searchTxOverlay, Iterate and GroupedAggregate all plan through — and
// TestPlanFor_MatchAllLeavesNoResidual covers BOTH spellings of match-all (the
// zero spi.Filter{} and the explicit empty AND), while
// TestPlanFor_EmptyOrIsNotMatchAll pins the asymmetry that makes the guard
// non-obvious: an empty AND is match-all, an empty OR is the false identity and
// must keep its residual.
//
// A version of that assertion used to live here, restricted to the empty AND,
// because the zero Filter{} never reaches planQuery in production and
// reconstructing the guard in-test asserted against a zero sqlPlan{} literal
// rather than anything production computed — it could not fail. Naming the
// guard removed that problem: planFor IS the production code, so the test
// exercises it directly.
