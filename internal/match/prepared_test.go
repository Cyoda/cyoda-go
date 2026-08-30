package match_test

import (
	"fmt"
	"testing"
	"time"

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
		{
			// Now field-independent: prepareLifecycle no longer intercepts
			// a temporal field's operator before expansion, so an
			// unsupported operator NAME (not merely one outside the
			// temporal allowlist) errors here exactly as it does on
			// "state" above, rather than being swallowed to a never-match.
			"IS_CHANGED on a temporal meta field",
			&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "IS_CHANGED"},
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

// TestPrepare_NeverMatchIsNotAnError pins the two data-leaf expansion
// failures that sit in FRONT of the error path: they are deliberate
// never-match behaviour (an operand that parses into no declared type is a
// leaf that never matches, not a structural fault) and turning either into a
// Prepare error would reject conditions that evaluate cleanly today.
//
// A temporal meta field's operator no longer has a case here: prepareLifecycle
// no longer intercepts before expansion (see TestEvaluatorsAgree_TemporalMetaField_StringOperator),
// so a string/pattern operator now genuinely evaluates against the
// RFC3339-bridged value instead of degrading to never-match, and an
// unsupported operator NAME is now a structural error
// (TestPrepare_StructuralErrors' "IS_CHANGED on a temporal meta field" case)
// exactly as it already was for every other meta field.
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
			"comparison leaf on an untyped path",
			&predicate.SimpleCondition{JsonPath: "$.unknown", OperatorType: "GREATER_THAN", Value: 5},
			func(string) []spi.DataType { return nil },
			[]byte(`{"unknown":10}`),
		},
		{
			"operand parses into no declared type",
			&predicate.SimpleCondition{JsonPath: "$.qty", OperatorType: "GREATER_THAN", Value: "not-a-number"},
			typed(spi.Integer),
			[]byte(`{"qty":10}`),
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

// TestPrepare_MalformedPathNeverMatches pins prepareSimple's
// spi.ParseFilterPath failure branch: a jsonPath outside the filter-path
// grammar becomes a never-match leaf, not a Prepare error — the same
// "never matches" answer an expansion failure already produces.
//
// This is defense in depth, not a reachable production path: both boundaries
// a condition passes through before reaching match.Prepare —
// search.ValidateConditionJSONPath for a search/delete condition, and the
// workflow criterion importer for a criterion — already reject a malformed
// jsonPath before it gets here. The branch is pinned anyway because it is
// live code with its own failure mode, not because a malformed path is
// expected to arrive.
func TestPrepare_MalformedPathNeverMatches(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.arr[", OperatorType: "NOT_NULL"}
	p, err := match.Prepare(cond, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil (never-match, not an error)", err)
	}
	if p.Match([]byte(`{"arr":[1,2,3]}`), spi.EntityMeta{}) {
		t.Error("Match() = true, want false: a malformed path must never match")
	}
}
