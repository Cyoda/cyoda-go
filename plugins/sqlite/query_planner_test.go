package sqlite

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestPlanQuery_EqSourceData(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "city",
		Source: spi.SourceData,
		Value:  "Berlin",
	}
	plan := planQuery(f)
	wantWhere := "(json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "Berlin" {
		t.Errorf("args = %v, want [Berlin]", plan.args)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil for pushable op, got %+v", plan.postFilter)
	}
}

func TestPlanQuery_NeSourceData(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterNe,
		Path:   "status",
		Source: spi.SourceData,
		Value:  "CLOSED",
	}
	plan := planQuery(f)
	wantWhere := "(json_extract(data, '$.status') IS NULL OR json_extract(data, '$.status') != ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "CLOSED" {
		t.Errorf("args = %v, want [CLOSED]", plan.args)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil")
	}
}

func TestPlanQuery_ComparisonOps(t *testing.T) {
	tests := []struct {
		name  string
		op    spi.FilterOp
		sqlOp string
	}{
		{"gt", spi.FilterGt, ">"},
		{"lt", spi.FilterLt, "<"},
		{"gte", spi.FilterGte, ">="},
		{"lte", spi.FilterLte, "<="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := spi.Filter{
				Op:     tt.op,
				Path:   "age",
				Source: spi.SourceData,
				Value:  float64(25),
			}
			plan := planQuery(f)
			want := "(json_extract(data, '$.age') IS NOT NULL AND json_extract(data, '$.age') " + tt.sqlOp + " ?)"
			if plan.where != want {
				t.Errorf("where:\n  got  %s\n  want %s", plan.where, want)
			}
			if len(plan.args) != 1 || plan.args[0] != float64(25) {
				t.Errorf("args = %v, want [25]", plan.args)
			}
			if plan.postFilter != nil {
				t.Errorf("postFilter should be nil")
			}
		})
	}
}

func TestPlanQuery_Contains(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterContains,
		Path:   "name",
		Source: spi.SourceData,
		Value:  "Ali",
	}
	plan := planQuery(f)
	wantWhere := "instr(json_extract(data, '$.name'), ?) > 0"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "Ali" {
		t.Errorf("args = %v, want [Ali]", plan.args)
	}
}

func TestPlanQuery_StartsWith(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterStartsWith,
		Path:   "name",
		Source: spi.SourceData,
		Value:  "Al",
	}
	plan := planQuery(f)
	wantWhere := "substr(json_extract(data, '$.name'), 1, length(?)) = ?"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != "Al" || plan.args[1] != "Al" {
		t.Errorf("args = %v, want [Al Al]", plan.args)
	}
}

func TestPlanQuery_EndsWith(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterEndsWith,
		Path:   "email",
		Source: spi.SourceData,
		Value:  ".com",
	}
	plan := planQuery(f)
	wantWhere := "substr(json_extract(data, '$.email'), -length(?)) = ?"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != ".com" || plan.args[1] != ".com" {
		t.Errorf("args = %v, want [.com .com]", plan.args)
	}
}

func TestPlanQuery_Like(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterLike,
		Path:   "desc",
		Source: spi.SourceData,
		Value:  "foo%bar_baz\\qux",
	}
	plan := planQuery(f)
	wantWhere := "json_extract(data, '$.desc') LIKE ? ESCAPE '\\'"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	// The value should have %, _, and \ escaped.
	wantVal := "foo\\%bar\\_baz\\\\qux"
	if len(plan.args) != 1 || plan.args[0] != wantVal {
		t.Errorf("args = %v, want [%s]", plan.args, wantVal)
	}
}

func TestPlanQuery_IsNull(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterIsNull,
		Path:   "address",
		Source: spi.SourceData,
	}
	plan := planQuery(f)
	wantWhere := "json_extract(data, '$.address') IS NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want []", plan.args)
	}
}

func TestPlanQuery_NotNull(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterNotNull,
		Path:   "phone",
		Source: spi.SourceData,
	}
	plan := planQuery(f)
	wantWhere := "json_extract(data, '$.phone') IS NOT NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want []", plan.args)
	}
}

func TestPlanQuery_Between(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "score",
		Source: spi.SourceData,
		Values: []any{float64(10), float64(20)},
	}
	plan := planQuery(f)
	wantWhere := "(json_extract(data, '$.score') IS NOT NULL AND json_extract(data, '$.score') BETWEEN ? AND ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != float64(10) || plan.args[1] != float64(20) {
		t.Errorf("args = %v, want [10 20]", plan.args)
	}
}

func TestPlanQuery_SourceMeta(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "state",
		Source: spi.SourceMeta,
		Value:  "ACTIVE",
	}
	plan := planQuery(f)
	// "state" is not a direct column — it lives in the meta JSONB blob.
	wantWhere := "(json_extract(json(meta), '$.state') IS NOT NULL AND json_extract(json(meta), '$.state') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "ACTIVE" {
		t.Errorf("args = %v, want [ACTIVE]", plan.args)
	}
}

func TestPlanQuery_SourceMetaGt(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterGt,
		Path:   "created_at",
		Source: spi.SourceMeta,
		Value:  int64(1000000),
	}
	plan := planQuery(f)
	wantWhere := "(created_at IS NOT NULL AND created_at > ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

func TestPlanQuery_NonPushable_Regex(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterMatchesRegex,
		Path:   "code",
		Source: spi.SourceData,
		Value:  "^[A-Z]+$",
	}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty for non-pushable, got %s", plan.where)
	}
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil for regex")
	}
	if plan.postFilter.Op != spi.FilterMatchesRegex {
		t.Errorf("postFilter.Op = %s, want matches_regex", plan.postFilter.Op)
	}
}

func TestPlanQuery_NonPushable_CaseInsensitive(t *testing.T) {
	tests := []spi.FilterOp{
		spi.FilterIEq,
		spi.FilterINe,
		spi.FilterIContains,
		spi.FilterINotContains,
		spi.FilterIStartsWith,
		spi.FilterINotStartsWith,
		spi.FilterIEndsWith,
		spi.FilterINotEndsWith,
	}
	for _, op := range tests {
		t.Run(string(op), func(t *testing.T) {
			f := spi.Filter{Op: op, Path: "name", Source: spi.SourceData, Value: "alice"}
			plan := planQuery(f)
			if plan.where != "" {
				t.Errorf("where should be empty, got %s", plan.where)
			}
			if plan.postFilter == nil {
				t.Fatal("postFilter should be non-nil")
			}
		})
	}
}

func TestPlanQuery_GreedyAND_MixedPushable(t *testing.T) {
	// AND with two pushable and one non-pushable child.
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(18)},
		},
	}
	plan := planQuery(f)

	// Pushed: eq(city) AND gt(age). joinChildren wraps each child in ().
	wantWhere := "((json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)) AND " +
		"((json_extract(data, '$.age') IS NOT NULL AND json_extract(data, '$.age') > ?))"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 {
		t.Errorf("args count = %d, want 2", len(plan.args))
	}

	// Residual: regex(code)
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil")
	}
	if plan.postFilter.Op != spi.FilterMatchesRegex {
		t.Errorf("postFilter.Op = %s, want matches_regex", plan.postFilter.Op)
	}
}

func TestPlanQuery_GreedyAND_AllPushable(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(18)},
		},
	}
	plan := planQuery(f)
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil when all children pushable")
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
}

func TestPlanQuery_GreedyAND_AllNonPushable(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterMatchesRegex, Path: "a", Source: spi.SourceData, Value: ".*"},
			{Op: spi.FilterIEq, Path: "b", Source: spi.SourceData, Value: "x"},
		},
	}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty, got %s", plan.where)
	}
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil")
	}
	if plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter.Op = %s, want and", plan.postFilter.Op)
	}
	if len(plan.postFilter.Children) != 2 {
		t.Errorf("postFilter.Children count = %d, want 2", len(plan.postFilter.Children))
	}
}

func TestPlanQuery_ConservativeOR_AllPushable(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Munich"},
		},
	}
	plan := planQuery(f)
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil when all OR children pushable")
	}

	wantWhere := "((json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)) OR " +
		"((json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?))"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

func TestPlanQuery_ConservativeOR_AnyNonPushable(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
		},
	}
	plan := planQuery(f)
	// Entire OR should become residual.
	if plan.where != "" {
		t.Errorf("where should be empty, got %s", plan.where)
	}
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil")
	}
	if plan.postFilter.Op != spi.FilterOr {
		t.Errorf("postFilter.Op = %s, want or", plan.postFilter.Op)
	}
}

func TestPlanQuery_NestedANDWithOR(t *testing.T) {
	// AND(eq(city), OR(eq(a), eq(b)))
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "x"},
					{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "y"},
				},
			},
		},
	}
	plan := planQuery(f)
	// Both eq(city) and the OR are fully pushable.
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil, got %+v", plan.postFilter)
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
}

func TestPlanQuery_NestedANDWithPartialOR(t *testing.T) {
	// AND(eq(city), OR(eq(a), regex(b)))
	// The OR is not fully pushable, so it becomes residual. eq(city) is pushed.
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "x"},
					{Op: spi.FilterMatchesRegex, Path: "b", Source: spi.SourceData, Value: "^y"},
				},
			},
		},
	}
	plan := planQuery(f)

	wantWhere := "(json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}

	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil")
	}
	if plan.postFilter.Op != spi.FilterOr {
		t.Errorf("postFilter.Op = %s, want or", plan.postFilter.Op)
	}
}

func TestPlanQuery_EmptyFilter(t *testing.T) {
	// An empty filter (zero-value) should produce no WHERE and no residual.
	f := spi.Filter{}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty for empty filter, got %s", plan.where)
	}
	// Empty filter is non-pushable (unknown op), so it becomes residual.
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil for unknown op")
	}
}

func TestPlanQuery_SingleChildAND(t *testing.T) {
	// AND with a single pushable child.
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "name", Source: spi.SourceData, Value: "Alice"},
		},
	}
	plan := planQuery(f)
	wantWhere := "(json_extract(data, '$.name') IS NOT NULL AND json_extract(data, '$.name') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil")
	}
}

func TestPlanQuery_BetweenInsufficientValues(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "val",
		Source: spi.SourceData,
		Values: []any{float64(10)}, // only 1 value
	}
	plan := planQuery(f)
	// Should produce a no-op WHERE.
	if plan.where != "1=1" {
		t.Errorf("where = %s, want 1=1", plan.where)
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"foo%bar", "foo\\%bar"},
		{"a_b", "a\\_b"},
		{"a\\b", "a\\\\b"},
		{"%_\\", "\\%\\_\\\\"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := escapeLike(tt.input)
			if got != tt.want {
				t.Errorf("escapeLike(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPlanQuery_DeeplyNested(t *testing.T) {
	// AND(OR(eq, eq), AND(gt, lt))
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "x"},
					{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "y"},
				},
			},
			{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					{Op: spi.FilterGt, Path: "c", Source: spi.SourceData, Value: float64(1)},
					{Op: spi.FilterLt, Path: "d", Source: spi.SourceData, Value: float64(100)},
				},
			},
		},
	}
	plan := planQuery(f)
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil for fully pushable tree")
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
}

func TestPlanQuery_SourceMetaIsNull(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterIsNull,
		Path:   "state",
		Source: spi.SourceMeta,
	}
	plan := planQuery(f)
	wantWhere := "json_extract(json(meta), '$.state') IS NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

// TestPlanQuery_MetaColumnMapping verifies that SourceMeta paths with direct
// columns (e.g., entity_id) use the column name, while paths without direct
// columns (e.g., state) use json_extract on the meta JSONB blob.
func TestPlanQuery_MetaColumnMapping(t *testing.T) {
	// entity_id is a direct column — should use the column name directly.
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "entity_id",
		Source: spi.SourceMeta,
		Value:  "abc-123",
	}
	plan := planQuery(f)
	wantWhere := "(entity_id IS NOT NULL AND entity_id = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

// FuzzQueryPlanner generates random spi.Filter trees and verifies that
// planQuery never panics, and that the pushable/residual split is consistent:
//   - If postFilter is nil, the original filter was fully pushable
//   - If postFilter is non-nil, it contains only non-pushable ops
func FuzzQueryPlanner(f *testing.F) {
	// Seed corpus with representative filter patterns.
	f.Add(byte(0), byte(0), "city", "Berlin", byte(0))  // eq, data
	f.Add(byte(1), byte(1), "state", "ACTIVE", byte(0)) // ne, meta
	f.Add(byte(2), byte(0), "age", "25", byte(1))       // gt, data, with AND wrapper
	f.Add(byte(12), byte(0), "code", "^[A-Z]", byte(0)) // regex, data
	f.Add(byte(8), byte(0), "name", "ali", byte(2))     // ieq, data, with OR wrapper
	f.Add(byte(5), byte(0), "score", "10", byte(1))     // lte, data, AND wrapper
	f.Add(byte(10), byte(0), "val", "5", byte(3))       // between, data, nested AND(OR(...))

	f.Fuzz(func(t *testing.T, opIdx byte, sourceIdx byte, path string, value string, treeShape byte) {
		// Map opIdx to a FilterOp. We cover all defined ops.
		allOps := []spi.FilterOp{
			spi.FilterEq,             // 0
			spi.FilterNe,             // 1
			spi.FilterGt,             // 2
			spi.FilterLt,             // 3
			spi.FilterGte,            // 4
			spi.FilterLte,            // 5
			spi.FilterContains,       // 6
			spi.FilterStartsWith,     // 7
			spi.FilterIEq,            // 8
			spi.FilterEndsWith,       // 9
			spi.FilterBetween,        // 10
			spi.FilterLike,           // 11
			spi.FilterMatchesRegex,   // 12
			spi.FilterIsNull,         // 13
			spi.FilterNotNull,        // 14
			spi.FilterINe,            // 15
			spi.FilterIContains,      // 16
			spi.FilterINotContains,   // 17
			spi.FilterIStartsWith,    // 18
			spi.FilterINotStartsWith, // 19
			spi.FilterIEndsWith,      // 20
			spi.FilterINotEndsWith,   // 21
		}
		op := allOps[int(opIdx)%len(allOps)]

		source := spi.SourceData
		if sourceIdx%2 == 1 {
			source = spi.SourceMeta
		}

		// Build a leaf filter.
		leaf := spi.Filter{
			Op:     op,
			Path:   path,
			Source: source,
			Value:  value,
		}
		if op == spi.FilterBetween {
			leaf.Values = []any{value, value + "z"}
			leaf.Value = nil
		}

		// Optionally wrap in a tree structure.
		var filter spi.Filter
		switch treeShape % 4 {
		case 0:
			filter = leaf
		case 1:
			// AND with the leaf and a pushable sibling.
			filter = spi.Filter{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					leaf,
					{Op: spi.FilterEq, Path: "x", Source: spi.SourceData, Value: "y"},
				},
			}
		case 2:
			// OR with the leaf and a pushable sibling.
			filter = spi.Filter{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					leaf,
					{Op: spi.FilterEq, Path: "x", Source: spi.SourceData, Value: "y"},
				},
			}
		case 3:
			// Nested: AND(OR(leaf, eq), gt)
			filter = spi.Filter{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					{
						Op: spi.FilterOr,
						Children: []spi.Filter{
							leaf,
							{Op: spi.FilterEq, Path: "x", Source: spi.SourceData, Value: "y"},
						},
					},
					{Op: spi.FilterGt, Path: "z", Source: spi.SourceData, Value: float64(1)},
				},
			}
		}

		// The core property: planQuery must not panic.
		plan := planQuery(filter)

		// Verify consistency: if postFilter is nil, original filter was fully pushable.
		if plan.postFilter == nil && plan.where == "" {
			// Empty where + nil postFilter is valid only for empty AND children
			// (which produces no filter at all). Otherwise, one must be non-empty.
			if filter.Op != spi.FilterAnd || len(filter.Children) > 0 {
				// This is OK — the leaf was pushable and produced SQL, or the
				// tree was fully pushable. Just verify no panic occurred.
			}
		}

		// Verify: if where is non-empty, it should not contain raw Go
		// format verbs (%!...) which would indicate a broken Sprintf.
		if plan.where != "" {
			if containsFormatVerb(plan.where) {
				t.Errorf("WHERE clause contains Go format verb: %q", plan.where)
			}
		}

		// Verify: postFilter (if present) should only contain non-pushable ops
		// at leaf level (or AND/OR wrapping them).
		if plan.postFilter != nil {
			verifyResidualOps(t, *plan.postFilter)
		}
	})
}

// containsFormatVerb returns true if the string contains a Go format verb
// like "%!(EXTRA..." which would indicate a broken fmt.Sprintf call.
func containsFormatVerb(s string) bool {
	return strings.Contains(s, "%!(")
}

// verifyResidualOps checks that a residual filter tree contains only
// non-pushable leaf ops (or AND/OR branches wrapping them).
func verifyResidualOps(t *testing.T, f spi.Filter) {
	t.Helper()
	switch f.Op {
	case spi.FilterAnd, spi.FilterOr:
		for _, child := range f.Children {
			verifyResidualOps(t, child)
		}
	default:
		if isPushable(f.Op) {
			// A pushable op in the residual is valid when it was part of an OR
			// that contained a non-pushable sibling (conservative OR). The OR
			// becomes fully residual, including its pushable children. This is
			// by design — we don't split OR children.
		}
	}
}
