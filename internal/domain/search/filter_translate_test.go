package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

func TestConditionToFilter_SimpleEquals(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatalf("ConditionToFilter: %v", err)
	}
	if f.Op != spi.FilterEq {
		t.Errorf("Op = %s, want eq", f.Op)
	}
	if f.Path != "name" {
		t.Errorf("Path = %s, want name", f.Path)
	}
	if f.Source != spi.SourceData {
		t.Errorf("Source = %s, want data", f.Source)
	}
	if f.Value != "Alice" {
		t.Errorf("Value = %v, want Alice", f.Value)
	}
}

func TestConditionToFilter_SimpleNoPrefix(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "city",
		OperatorType: "EQUALS",
		Value:        "Berlin",
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "city" {
		t.Errorf("Path = %s, want city", f.Path)
	}
}

func TestConditionToFilter_SimpleNestedPath(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.address.city",
		OperatorType: "NOT_EQUAL",
		Value:        "Berlin",
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterNe {
		t.Errorf("Op = %s, want ne", f.Op)
	}
	if f.Path != "address.city" {
		t.Errorf("Path = %s, want address.city", f.Path)
	}
}

func TestConditionToFilter_AllSimpleOperators(t *testing.T) {
	tests := []struct {
		op   string
		want spi.FilterOp
	}{
		{"EQUALS", spi.FilterEq},
		{"NOT_EQUAL", spi.FilterNe},
		{"GREATER_THAN", spi.FilterGt},
		{"LESS_THAN", spi.FilterLt},
		{"GREATER_OR_EQUAL", spi.FilterGte},
		{"LESS_OR_EQUAL", spi.FilterLte},
		{"CONTAINS", spi.FilterContains},
		{"STARTS_WITH", spi.FilterStartsWith},
		{"ENDS_WITH", spi.FilterEndsWith},
		{"LIKE", spi.FilterLike},
		{"IS_NULL", spi.FilterIsNull},
		{"NOT_NULL", spi.FilterNotNull},
		{"BETWEEN", spi.FilterBetween},
		{"MATCHES_PATTERN", spi.FilterMatchesRegex},
		{"IEQUALS", spi.FilterIEq},
		{"ICONTAINS", spi.FilterIContains},
		{"ISTARTS_WITH", spi.FilterIStartsWith},
		{"IENDS_WITH", spi.FilterIEndsWith},
	}

	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			cond := &predicate.SimpleCondition{
				JsonPath:     "$.field",
				OperatorType: tt.op,
				Value:        "val",
			}
			f, err := ConditionToFilter(cond, nil)
			if err != nil {
				t.Fatal(err)
			}
			if f.Op != tt.want {
				t.Errorf("Op = %s, want %s", f.Op, tt.want)
			}
		})
	}
}

func TestConditionToFilter_UnknownOperator(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.field",
		OperatorType: "SOME_UNKNOWN_OP",
		Value:        "val",
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown operators map to matches_regex to force post-filtering.
	if f.Op != spi.FilterMatchesRegex {
		t.Errorf("Op = %s, want matches_regex for unknown op", f.Op)
	}
}

func TestConditionToFilter_Lifecycle(t *testing.T) {
	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "EQUALS",
		Value:        "ACTIVE",
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterEq {
		t.Errorf("Op = %s, want eq", f.Op)
	}
	if f.Source != spi.SourceMeta {
		t.Errorf("Source = %s, want meta", f.Source)
	}
	if f.Path != "state" {
		t.Errorf("Path = %s, want state", f.Path)
	}
	if f.Value != "ACTIVE" {
		t.Errorf("Value = %v, want ACTIVE", f.Value)
	}
}

func TestConditionToFilter_GroupAND(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
			&predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "GREATER_THAN", Value: float64(25)},
		},
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterAnd {
		t.Errorf("Op = %s, want and", f.Op)
	}
	if len(f.Children) != 2 {
		t.Fatalf("Children count = %d, want 2", len(f.Children))
	}
	if f.Children[0].Op != spi.FilterEq {
		t.Errorf("Children[0].Op = %s, want eq", f.Children[0].Op)
	}
	if f.Children[1].Op != spi.FilterGt {
		t.Errorf("Children[1].Op = %s, want gt", f.Children[1].Op)
	}
}

func TestConditionToFilter_GroupOR(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "OR",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.city", OperatorType: "EQUALS", Value: "Berlin"},
			&predicate.SimpleCondition{JsonPath: "$.city", OperatorType: "EQUALS", Value: "Munich"},
		},
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterOr {
		t.Errorf("Op = %s, want or", f.Op)
	}
	if len(f.Children) != 2 {
		t.Fatalf("Children count = %d, want 2", len(f.Children))
	}
}

func TestConditionToFilter_NestedGroup(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.active", OperatorType: "EQUALS", Value: true},
			&predicate.GroupCondition{
				Operator: "OR",
				Conditions: []predicate.Condition{
					&predicate.SimpleCondition{JsonPath: "$.city", OperatorType: "EQUALS", Value: "Berlin"},
					&predicate.SimpleCondition{JsonPath: "$.city", OperatorType: "EQUALS", Value: "Munich"},
				},
			},
		},
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterAnd {
		t.Errorf("Op = %s, want and", f.Op)
	}
	if len(f.Children) != 2 {
		t.Fatalf("Children count = %d, want 2", len(f.Children))
	}
	if f.Children[1].Op != spi.FilterOr {
		t.Errorf("Children[1].Op = %s, want or", f.Children[1].Op)
	}
}

func TestConditionToFilter_Array(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.tags",
		Values:   []any{"go", nil, "test"},
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Array conditions expand to AND of positional equality checks.
	// Values: ["go", nil, "test"] → tags.0 = "go" AND tags.2 = "test"
	if f.Op != spi.FilterAnd {
		t.Errorf("Op = %s, want and for array condition", f.Op)
	}
	if len(f.Children) != 2 {
		t.Fatalf("Children count = %d, want 2 (nil positions skipped)", len(f.Children))
	}
	if f.Children[0].Path != "tags.0" {
		t.Errorf("Children[0].Path = %s, want tags.0", f.Children[0].Path)
	}
	if f.Children[0].Op != spi.FilterEq {
		t.Errorf("Children[0].Op = %s, want eq", f.Children[0].Op)
	}
	if f.Children[0].Value != "go" {
		t.Errorf("Children[0].Value = %v, want go", f.Children[0].Value)
	}
	if f.Children[1].Path != "tags.2" {
		t.Errorf("Children[1].Path = %s, want tags.2", f.Children[1].Path)
	}
	if f.Children[1].Value != "test" {
		t.Errorf("Children[1].Value = %v, want test", f.Children[1].Value)
	}
}

func TestConditionToFilter_ArraySingleValue(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.items",
		Values:   []any{nil, "only"},
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Single non-nil value should produce a bare eq filter (no AND wrapper).
	if f.Op != spi.FilterEq {
		t.Errorf("Op = %s, want eq for single-value array", f.Op)
	}
	if f.Path != "items.1" {
		t.Errorf("Path = %s, want items.1", f.Path)
	}
}

func TestConditionToFilter_ArrayAllNil(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.arr",
		Values:   []any{nil, nil},
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatal(err)
	}
	// All-nil values produce an empty AND (tautology — matches everything).
	if f.Op != spi.FilterAnd {
		t.Errorf("Op = %s, want and for all-nil array", f.Op)
	}
	if len(f.Children) != 0 {
		t.Errorf("Children count = %d, want 0", len(f.Children))
	}
}

func TestConditionToFilter_Function(t *testing.T) {
	cond := &predicate.FunctionCondition{}
	_, err := ConditionToFilter(cond, nil)
	if err == nil {
		t.Fatal("expected error for FunctionCondition, got nil")
	}
}

func TestConditionToFilter_Nil(t *testing.T) {
	_, err := ConditionToFilter(nil, nil)
	if err == nil {
		t.Fatal("expected error for nil condition, got nil")
	}
}

// TestConditionToFilter_WildcardPath_ReturnsError verifies that paths
// containing JSONPath array-wildcard or subscript syntax (e.g. "[*]", "[0]")
// cause ConditionToFilter to return an error so the search service falls back
// to in-memory evaluation. Such paths cannot be translated to pushdown filters.
func TestConditionToFilter_WildcardPath_ReturnsError(t *testing.T) {
	wildcardPaths := []string{
		"$.items[*].name",
		"$.arr[0].field",
		"$.foo[*]",
	}
	for _, path := range wildcardPaths {
		cond := &predicate.SimpleCondition{
			JsonPath:     path,
			OperatorType: "EQUALS",
			Value:        "x",
		}
		_, err := ConditionToFilter(cond, nil)
		if err == nil {
			t.Errorf("ConditionToFilter with path %q: expected error (non-pushdownable), got nil", path)
		}
	}
}

// TestConditionToFilter_HyphenatedPath_Accepted verifies that hyphenated
// field names (e.g. "some-array", "some-object") are accepted by
// ConditionToFilter — they are valid JSON key characters and safe for
// storage backend pushdown.
func TestConditionToFilter_HyphenatedPath_Accepted(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.some-array.some-object",
		OperatorType: "EQUALS",
		Value:        "abc",
	}
	f, err := ConditionToFilter(cond, nil)
	if err != nil {
		t.Fatalf("ConditionToFilter with hyphenated path: unexpected error: %v", err)
	}
	if f.Path != "some-array.some-object" {
		t.Errorf("Path = %q, want some-array.some-object", f.Path)
	}
}

// TestConditionToFilter_StampsTemporalMeta verifies that a lifecycle
// condition against a known temporal meta field (creationDate) stamps
// Filter.Coercion = CoerceTemporal so storage plugins compare it as
// floored epoch-millis rather than lexicographically.
func TestConditionToFilter_StampsTemporalMeta(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_THAN", Value: "2021-01-01T00:00:00Z"}
	f, err := ConditionToFilter(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Coercion != spi.CoerceTemporal {
		t.Errorf("creationDate leaf Coercion = %v, want CoerceTemporal", f.Coercion)
	}
}

// TestConditionToFilter_DataLeafStampsNone verifies that a data-field leaf
// without a schema FieldsMap stamps Filter.Coercion = CoerceNone (no
// classification information available → default, non-temporal comparison).
func TestConditionToFilter_DataLeafStampsNone(t *testing.T) {
	c := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "x"}
	f, _ := ConditionToFilter(c, nil) // no schema → CoerceNone
	if f.Coercion != spi.CoerceNone {
		t.Errorf("data leaf Coercion = %v, want CoerceNone", f.Coercion)
	}
}

// TestConditionToFilter_SimpleBetween_PopulatesValues verifies that a
// BETWEEN SimpleCondition (data leaf) populates Filter.Values with the two
// bounds. Every downstream BETWEEN consumer (spi.evalLeafFilter,
// postgres/sqlite query planners) reads Filter.Values, not Filter.Value —
// leaving Values unset means BETWEEN silently never matches.
func TestConditionToFilter_SimpleBetween_PopulatesValues(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        []any{float64(18), float64(65)},
	}
	f, err := ConditionToFilter(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterBetween {
		t.Fatalf("Op = %s, want between", f.Op)
	}
	if len(f.Values) != 2 {
		t.Fatalf("Values = %v, want 2-element slice [18, 65]", f.Values)
	}
	if f.Values[0] != float64(18) || f.Values[1] != float64(65) {
		t.Errorf("Values = %v, want [18 65]", f.Values)
	}
}

// TestConditionToFilter_LifecycleBetween_PopulatesValues verifies that a
// BETWEEN LifecycleCondition on a temporal meta field (creationDate)
// populates Filter.Values with the two bounds AND stamps CoerceTemporal, so
// storage-plugin BETWEEN pushdown and spi.MatchFilter can actually match.
func TestConditionToFilter_LifecycleBetween_PopulatesValues(t *testing.T) {
	c := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2021-01-01T00:00:00Z", "2021-12-31T00:00:00Z"},
	}
	f, err := ConditionToFilter(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != spi.FilterBetween {
		t.Fatalf("Op = %s, want between", f.Op)
	}
	if f.Source != spi.SourceMeta {
		t.Errorf("Source = %s, want meta", f.Source)
	}
	if f.Coercion != spi.CoerceTemporal {
		t.Errorf("Coercion = %v, want CoerceTemporal", f.Coercion)
	}
	if len(f.Values) != 2 {
		t.Fatalf("Values = %v, want 2-element slice", f.Values)
	}
	if f.Values[0] != "2021-01-01T00:00:00Z" || f.Values[1] != "2021-12-31T00:00:00Z" {
		t.Errorf("Values = %v, want [2021-01-01T00:00:00Z 2021-12-31T00:00:00Z]", f.Values)
	}
}

// TestConditionToFilter_SimpleBetween_MalformedValue_LeavesValuesNil verifies
// that a malformed BETWEEN value (not a 2-element []any) leaves Filter.Values
// nil rather than panicking — validation elsewhere rejects malformed BETWEEN
// conditions, and a nil Values correctly no-matches downstream.
func TestConditionToFilter_SimpleBetween_MalformedValue_LeavesValuesNil(t *testing.T) {
	c := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        "not-a-slice",
	}
	f, err := ConditionToFilter(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Values != nil {
		t.Errorf("Values = %v, want nil for malformed BETWEEN value", f.Values)
	}
}
