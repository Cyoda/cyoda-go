package match_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// typed is a FieldTypes that declares every path it is asked about as t.
func typed(t ...spi.DataType) match.FieldTypes {
	return func(string) []spi.DataType { return t }
}

// TestPrepare_StructuralErrors pins the five faults that move from per-row
// evaluation into Prepare. Each keeps its exact message so no error mapping
// moves — these are reported from the condition's own shape now, not from
// which rows happen to be present.
func TestPrepare_StructuralErrors(t *testing.T) {
	tests := []struct {
		name string
		cond predicate.Condition
		want string
	}{
		{
			"function condition",
			&predicate.FunctionCondition{},
			"function conditions not implemented",
		},
		{
			"function condition nested in a group",
			&predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{
				&predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"},
				&predicate.FunctionCondition{},
			}},
			"function conditions not implemented",
		},
		{
			"unknown lifecycle field",
			&predicate.LifecycleCondition{Field: "nosuchfield", OperatorType: "EQUALS", Value: "x"},
			"unknown lifecycle field: nosuchfield",
		},
		{
			"unknown group operator",
			&predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
				&predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"},
			}},
			"unknown group operator: NOT",
		},
		{
			"lowercase group operator",
			&predicate.GroupCondition{Operator: "or", Conditions: nil},
			"unknown group operator: or",
		},
		{
			"unsupported operator name on a data leaf",
			&predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "FROBNICATE", Value: 1},
			"unsupported operator: FROBNICATE",
		},
		{
			"IS_CHANGED on a non-temporal meta field",
			&predicate.LifecycleCondition{Field: "state", OperatorType: "IS_CHANGED"},
			"unsupported operator: IS_CHANGED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := match.Prepare(tc.cond, typed(spi.Integer))
			if err == nil {
				t.Fatalf("Prepare() = nil error, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Prepare() error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPrepare_NeverMatchIsNotAnError pins the two cases that sit in FRONT of
// the error path: they are deliberate never-match behaviour (the
// prepareLifecycle temporal-meta guard) and turning either into a Prepare
// error would silently reactivate a dormant workflow-criterion transition on
// a binary upgrade alone (see the guard's own doc in prepared.go).
//
// The other two cases this table used to carry — a comparison leaf on an
// untyped path, and an operand that parses into no declared type — moved to
// TestPrepare_UnevaluableLeafIsAnError: both are leafNode's expansion-failure
// branch, which now fails Prepare instead of building a silent never-match
// leaf.
func TestPrepare_NeverMatchIsNotAnError(t *testing.T) {
	meta := spi.EntityMeta{
		State:        "active",
		CreationDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name       string
		cond       predicate.Condition
		fieldTypes match.FieldTypes
		data       []byte
	}{
		{
			// Field-dependent, not operator-dependent: the same operator on
			// `state` IS an error (covered above).
			"IS_CHANGED on a temporal meta field",
			&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "IS_CHANGED"},
			nil,
			[]byte(`{}`),
		},
		{
			"CONTAINS on a temporal meta field",
			&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2026"},
			nil,
			[]byte(`{}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := match.Prepare(tc.cond, tc.fieldTypes)
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil (never-match, not an error)", err)
			}
			if p.Match(tc.data, meta) {
				t.Error("Match() = true, want false (never-match leaf)")
			}
		})
	}
}

// TestPrepare_ZeroValueNeverMatches pins that the zero Prepared fails closed.
// Prepare returns it alongside an error, and a caller that ignored the error
// must not get a match-all.
func TestPrepare_ZeroValueNeverMatches(t *testing.T) {
	var p match.Prepared
	if p.Match([]byte(`{"a":1}`), spi.EntityMeta{}) {
		t.Error("zero Prepared.Match() = true, want false (fail closed)")
	}
}

// TestPrepare_PreviousTransitionCanonicalisedBeforeFieldCheck pins the
// ordering trap: previousTransition must be rewritten to
// transitionForLatestSave BEFORE the unknown-field check, or a working field
// name starts erroring.
func TestPrepare_PreviousTransitionCanonicalisedBeforeFieldCheck(t *testing.T) {
	cond := &predicate.LifecycleCondition{
		Field: "previousTransition", OperatorType: "EQUALS", Value: "approve",
	}
	p, err := match.Prepare(cond, nil)
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if !p.Match([]byte(`{}`), spi.EntityMeta{TransitionForLatestSave: "approve"}) {
		t.Error("Match() = false, want true")
	}
	if p.Match([]byte(`{}`), spi.EntityMeta{TransitionForLatestSave: "reject"}) {
		t.Error("Match() = true for a non-equal transition, want false")
	}
}

// TestPrepare_ArrayConditionOneExpansionPerPosition pins that an
// ArrayCondition resolves one expansion per NON-NIL position — each position
// is an EQUALS with its own operand, so a single leaf-level expansion cannot
// serve them. Nil positions are skipped, and an all-nil condition matches.
func TestPrepare_ArrayConditionOneExpansionPerPosition(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.tags",
		Values:   []any{"red", nil, "blue"},
	}
	p, err := match.Prepare(cond, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !p.Match([]byte(`{"tags":["red","anything","blue"]}`), spi.EntityMeta{}) {
		t.Error("Match() = false, want true: positions 0 and 2 match, 1 is skipped")
	}
	if p.Match([]byte(`{"tags":["red","anything","green"]}`), spi.EntityMeta{}) {
		t.Error("Match() = true, want false: position 2 does not match")
	}

	allNil := &predicate.ArrayCondition{JsonPath: "$.tags", Values: []any{nil, nil}}
	pn, err := match.Prepare(allNil, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !pn.Match([]byte(`{"tags":[]}`), spi.EntityMeta{}) {
		t.Error("Match() = false for an all-nil array condition, want true")
	}
}

// TestPrepare_ArrayWildcardRoutesPerRow pins that a "[*]" hop is resolved
// PER ROW: the parsed hops are fixed at Prepare time, but spi.ResolvePath
// walks them against each row's own data, so the same prepared leaf answers
// correctly whether that row's "laureates" array is populated or empty. This
// is syntax-driven (the "[*]" in the path), never data-driven — a bare path
// with no wildcard would NOT iterate an array's elements, however the stored
// value is shaped.
func TestPrepare_ArrayWildcardRoutesPerRow(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath: "$.laureates[*].motivation", OperatorType: "CONTAINS", Value: "peace",
	}
	p, err := match.Prepare(cond, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !p.Match([]byte(`{"laureates":[{"motivation":"for war"},{"motivation":"for peace"}]}`), spi.EntityMeta{}) {
		t.Error("Match() = false, want true: one element matches")
	}
	if p.Match([]byte(`{"laureates":[{"motivation":"for war"}]}`), spi.EntityMeta{}) {
		t.Error("Match() = true, want false: no element matches")
	}
	if p.Match([]byte(`{"laureates":[]}`), spi.EntityMeta{}) {
		t.Error("Match() = true for an empty array, want false")
	}
}

// unknownCondition implements predicate.Condition without being one of the
// four kinds prepare() recognises (SimpleCondition, LifecycleCondition,
// GroupCondition, FunctionCondition) — the one structural fault
// TestPrepare_StructuralErrors does not cover, since every real
// predicate.Condition implementation in this repo falls into one of those
// four. spi.DesugarCondition, which prepare() calls first, passes an
// unrecognised type through unchanged (its own switch only rewrites
// ArrayCondition and recurses into GroupCondition), so this reaches
// prepare()'s default: arm exactly as if DesugarCondition were not called.
type unknownCondition struct{}

func (unknownCondition) Type() string { return "unknown" }

// TestPrepare_UnknownConditionType pins the fifth structural fault: a
// Condition implementation outside the four kinds prepare()'s type switch
// recognises.
func TestPrepare_UnknownConditionType(t *testing.T) {
	cond := unknownCondition{}
	want := fmt.Sprintf("unknown condition type: %T", cond)

	_, err := match.Prepare(cond, nil)
	if err == nil {
		t.Fatalf("Prepare() = nil error, want %q", want)
	}
	if err.Error() != want {
		t.Errorf("Prepare() error = %q, want %q", err.Error(), want)
	}
}

// TestPrepare_MalformedPathIsAnError pins prepareSimple's spi.ParseFilterPath
// failure branch: a jsonPath outside the filter-path grammar fails Prepare —
// the same fault class an expansion failure already is (see
// TestPrepare_UnevaluableLeafIsAnError), not a silent never-match leaf.
//
// This is defense in depth, not a reachable production path: both boundaries
// a condition passes through before reaching match.Prepare —
// search.ValidateConditionJSONPath for a search/delete condition, and the
// workflow criterion importer for a criterion — already reject a malformed
// jsonPath before it gets here. The branch is pinned anyway because it is
// live code with its own failure mode, not because a malformed path is
// expected to arrive.
func TestPrepare_MalformedPathIsAnError(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.arr[", OperatorType: "NOT_NULL"}
	_, err := match.Prepare(cond, typed(spi.String))
	require.Error(t, err)
}

// TestPrepare_EmptyLeafPathIsAnError mirrors the SPI kernel's own guard
// (prepareNode in prepared_filter.go): a SourceData LEAF with an empty path
// addresses no field and cannot be evaluated. Without this guard,
// stripLeader("$") / stripLeader("$.") / stripLeader("") all yield "",
// spi.ParseFilterPath("") legitimately returns (nil, nil) — the shape a TREE
// operator's absent path is allowed to take — and spi.ResolvePath(data, nil)
// resolves that nil hop slice to the parsed ROOT DOCUMENT, so a presence
// test (NOT_NULL) would match every entity regardless of its shape were this
// not rejected at Prepare time.
func TestPrepare_EmptyLeafPathIsAnError(t *testing.T) {
	paths := []string{"", "$", "$."}
	ops := []struct {
		op    string
		value any
	}{
		{"NOT_NULL", nil},
		{"CONTAINS", "needle"},
	}
	for _, path := range paths {
		for _, o := range ops {
			t.Run(fmt.Sprintf("path=%q/%s", path, o.op), func(t *testing.T) {
				cond := &predicate.SimpleCondition{JsonPath: path, OperatorType: o.op, Value: o.value}
				_, err := match.Prepare(cond, typed(spi.String))
				require.Error(t, err)
			})
		}
	}
}

// TestPrepare_UnevaluableLeafIsAnError pins the three swallows that move from
// never-match into a Prepare error: an operand that fits no declared type
// (leafNode's expansion-failure branch), an empty leaf path, and a jsonPath
// outside the filter-path grammar (both prepareSimple branches). Each used to
// produce a leaf that silently never matched; each is now a structural fault
// in the CONDITION and fails Prepare instead.
func TestPrepare_UnevaluableLeafIsAnError(t *testing.T) {
	strTypes := func(string) []spi.DataType { return []spi.DataType{spi.Integer} }
	cases := []struct {
		name string
		cond predicate.Condition
		ft   match.FieldTypes
	}{
		{"operand fits no declared type",
			&predicate.SimpleCondition{JsonPath: "$.n", OperatorType: "GREATER_THAN", Value: "abc"}, strTypes},
		{"empty path",
			&predicate.SimpleCondition{JsonPath: "$.", OperatorType: "EQUALS", Value: "x"}, strTypes},
		{"path outside the grammar",
			&predicate.SimpleCondition{JsonPath: "$.a[", OperatorType: "EQUALS", Value: "x"}, strTypes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := match.Prepare(c.cond, c.ft)
			require.Error(t, err)
		})
	}
}

// TestPrepare_ComparisonLeafOnUntypedPathIsAnError pins the same
// expansion-failure branch as TestPrepare_UnevaluableLeafIsAnError, but for a
// NIL declared-types set rather than a declared set the operand merely
// doesn't fit — the shape a caller gets when the path names a field
// FieldTypes has no entry for at all.
func TestPrepare_ComparisonLeafOnUntypedPathIsAnError(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.unknown", OperatorType: "GREATER_THAN", Value: 5}
	_, err := match.Prepare(cond, func(string) []spi.DataType { return nil })
	require.Error(t, err)
}

// TestPrepare_TemporalMetaGuardStaysANonMatch pins the fourth swallow — the
// temporal-meta guard in prepareLifecycle — as the one that does NOT become
// an error. operator-semantics.md and prepared_equivalence_test.go pin this
// as a deliberate, permanent never-match: relaxing it would silently
// reactivate a dormant transition in a stored workflow criterion on a binary
// upgrade alone.
func TestPrepare_TemporalMetaGuardStaysANonMatch(t *testing.T) {
	p, err := match.Prepare(&predicate.LifecycleCondition{
		Field: "creationDate", OperatorType: "CONTAINS", Value: "2024"}, nil)
	require.NoError(t, err)
	require.False(t, p.Match([]byte(`{}`), spi.EntityMeta{CreationDate: time.Now()}))
}
