package postgres

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
	wantWhere := "(doc->>'city' IS NOT NULL AND doc->>'city' = $1)"
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
	wantWhere := "(doc->>'status' IS NULL OR doc->>'status' != $1)"
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

func TestPlanQuery_ComparisonOps_String(t *testing.T) {
	// String values use plain text comparison.
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
				Path:   "name",
				Source: spi.SourceData,
				Value:  "M",
			}
			plan := planQuery(f)
			want := "(doc->>'name' IS NOT NULL AND doc->>'name' " + tt.sqlOp + " $1)"
			if plan.where != want {
				t.Errorf("where:\n  got  %s\n  want %s", plan.where, want)
			}
			if len(plan.args) != 1 || plan.args[0] != "M" {
				t.Errorf("args = %v, want [M]", plan.args)
			}
		})
	}
}

func TestPlanQuery_ComparisonOps_Numeric(t *testing.T) {
	// Numeric values route through cyoda_try_float8 with ::float8 cast.
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
			want := "(cyoda_try_float8(doc->>'age') IS NOT NULL AND cyoda_try_float8(doc->>'age') " + tt.sqlOp + " $1::float8)"
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
	// strpos returns 0 when not found and >0 when found — matches sqlite's instr semantics.
	wantWhere := "strpos(doc->>'name', $1) > 0"
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
	wantWhere := "substr(doc->>'name', 1, length($1)) = $2"
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
	wantWhere := "substr(doc->>'email', -length($1)) = $2"
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
	wantWhere := "doc->>'desc' LIKE $1 ESCAPE '\\'"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
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
	wantWhere := "doc->>'address' IS NULL"
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
	wantWhere := "doc->>'phone' IS NOT NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want []", plan.args)
	}
}

func TestPlanQuery_Between_String(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "name",
		Source: spi.SourceData,
		Values: []any{"A", "Z"},
	}
	plan := planQuery(f)
	wantWhere := "(doc->>'name' IS NOT NULL AND doc->>'name' BETWEEN $1 AND $2)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != "A" || plan.args[1] != "Z" {
		t.Errorf("args = %v, want [A Z]", plan.args)
	}
}

func TestPlanQuery_Between_Numeric(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "score",
		Source: spi.SourceData,
		Values: []any{float64(10), float64(20)},
	}
	plan := planQuery(f)
	wantWhere := "(cyoda_try_float8(doc->>'score') IS NOT NULL AND cyoda_try_float8(doc->>'score') BETWEEN $1::float8 AND $2::float8)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != float64(10) || plan.args[1] != float64(20) {
		t.Errorf("args = %v, want [10 20]", plan.args)
	}
}

func TestPlanQuery_SourceMeta_State(t *testing.T) {
	// "state" lives inside doc->'_meta' (not a direct column on entities).
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "state",
		Source: spi.SourceMeta,
		Value:  "ACTIVE",
	}
	plan := planQuery(f)
	wantWhere := "(doc->'_meta'->>'state' IS NOT NULL AND doc->'_meta'->>'state' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "ACTIVE" {
		t.Errorf("args = %v, want [ACTIVE]", plan.args)
	}
}

func TestPlanQuery_SourceMeta_DirectColumn(t *testing.T) {
	// entity_id is a direct column on the entities table.
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "entity_id",
		Source: spi.SourceMeta,
		Value:  "abc-123",
	}
	plan := planQuery(f)
	wantWhere := "(entity_id IS NOT NULL AND entity_id = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

func TestPlanQuery_NestedPath(t *testing.T) {
	// Dotted paths map to chained -> followed by ->> on the leaf.
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "parent.child",
		Source: spi.SourceData,
		Value:  "x",
	}
	plan := planQuery(f)
	wantWhere := "(doc->'parent'->>'child' IS NOT NULL AND doc->'parent'->>'child' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

func TestPlanQuery_NestedPath_TwoLevels(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterEq,
		Path:   "a.b.c",
		Source: spi.SourceData,
		Value:  "x",
	}
	plan := planQuery(f)
	wantWhere := "(doc->'a'->'b'->>'c' IS NOT NULL AND doc->'a'->'b'->>'c' = $1)"
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
	// Mirror sqlite: case-insensitive ops are NOT pushable (residual).
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
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
			{Op: spi.FilterEq, Path: "country", Source: spi.SourceData, Value: "DE"},
		},
	}
	plan := planQuery(f)

	wantWhere := "((doc->>'city' IS NOT NULL AND doc->>'city' = $1)) AND " +
		"((doc->>'country' IS NOT NULL AND doc->>'country' = $2))"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 {
		t.Errorf("args count = %d, want 2", len(plan.args))
	}

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
			{Op: spi.FilterEq, Path: "country", Source: spi.SourceData, Value: "DE"},
		},
	}
	plan := planQuery(f)
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil when all children pushable")
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
	if len(plan.args) != 2 {
		t.Errorf("args count = %d, want 2", len(plan.args))
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
	wantWhere := "((doc->>'city' IS NOT NULL AND doc->>'city' = $1)) OR " +
		"((doc->>'city' IS NOT NULL AND doc->>'city' = $2))"
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
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil, got %+v", plan.postFilter)
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
}

func TestPlanQuery_NestedANDWithPartialOR(t *testing.T) {
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
	wantWhere := "(doc->>'city' IS NOT NULL AND doc->>'city' = $1)"
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
	f := spi.Filter{}
	plan := planQuery(f)
	if plan.where != "" {
		t.Errorf("where should be empty for empty filter, got %s", plan.where)
	}
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil for unknown op")
	}
}

func TestPlanQuery_SingleChildAND(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "name", Source: spi.SourceData, Value: "Alice"},
		},
	}
	plan := planQuery(f)
	wantWhere := "(doc->>'name' IS NOT NULL AND doc->>'name' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil")
	}
}

// C1/M4 — a malformed (non-2-element) BETWEEN value must fail closed
// (exclude every row), matching memory's spi.MatchFilter behavior, not
// match-all ("1=1"). Validation now rejects this upstream (see
// internal/domain/search/operators.go validateBetweenArity), so this guard
// is defense-in-depth for any Filter constructed directly (bypassing the
// domain validator), never a panic and never a false match-all.
func TestPlanQuery_BetweenInsufficientValues(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "val",
		Source: spi.SourceData,
		Values: []any{float64(10)},
	}
	plan := planQuery(f)
	if plan.where != "false" {
		t.Errorf("where = %s, want false (exclude, matching memory's fail-closed semantics)", plan.where)
	}
}

// TestPlan_TemporalBetween_NilValues_DoesNotPanic is the C1 regression test:
// a CoerceTemporal BETWEEN leaf with Values=nil (the shape produced by
// betweenValues for a malformed BETWEEN operand, before the fix landed) must
// not panic indexing f.Values[0]/[1], and must emit an exclude predicate —
// never the sqlite/memory-diverging match-all.
func TestPlan_TemporalBetween_NilValues_DoesNotPanic(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: nil,
	}
	plan := planQuery(f)
	if plan.where != "false" {
		t.Errorf("where = %s, want false (exclude) for a malformed temporal BETWEEN with no values", plan.where)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want none", plan.args)
	}
}

// TestPlan_TemporalBetween_OneValue_DoesNotPanic covers the 1-element-array
// shape of the same malformed condition.
func TestPlan_TemporalBetween_OneValue_DoesNotPanic(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: []any{"2021-01-01T00:00:00Z"},
	}
	plan := planQuery(f)
	if plan.where != "false" {
		t.Errorf("where = %s, want false (exclude) for a malformed temporal BETWEEN with one value", plan.where)
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

func TestPlanQuery_PlaceholderNumbering(t *testing.T) {
	// Verify $1, $2, $3 increase across multiple args in a tree.
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "1"},
			{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "2"},
			{Op: spi.FilterEq, Path: "c", Source: spi.SourceData, Value: "3"},
		},
	}
	plan := planQuery(f)
	// Must contain $1, $2, $3 in order, and only those.
	if !strings.Contains(plan.where, "$1") || !strings.Contains(plan.where, "$2") || !strings.Contains(plan.where, "$3") {
		t.Errorf("expected $1/$2/$3 in where: %s", plan.where)
	}
	if strings.Contains(plan.where, "$4") {
		t.Errorf("unexpected $4 in where: %s", plan.where)
	}
	if len(plan.args) != 3 {
		t.Errorf("args count = %d, want 3", len(plan.args))
	}
}

func TestPlanQuery_StartsWith_PlaceholderReuse(t *testing.T) {
	// StartsWith uses the value twice (length($N) = $N+1). Verify that when
	// combined with another filter, numbering continues correctly.
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterStartsWith, Path: "a", Source: spi.SourceData, Value: "x"},
			{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "y"},
		},
	}
	plan := planQuery(f)
	wantWhere := "(substr(doc->>'a', 1, length($1)) = $2) AND ((doc->>'b' IS NOT NULL AND doc->>'b' = $3))"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 3 {
		t.Errorf("args count = %d, want 3", len(plan.args))
	}
}

func TestPlanQuery_DeeplyNested(t *testing.T) {
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
		t.Errorf("postFilter should be nil for fully pushable tree, got %+v", plan.postFilter)
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
}

func TestPlanQuery_SourceMeta_StateIsNull(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterIsNull,
		Path:   "state",
		Source: spi.SourceMeta,
	}
	plan := planQuery(f)
	wantWhere := "doc->'_meta'->>'state' IS NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

// TestPlanQuery_IsPushableParityWithSqlite asserts the set of ops we mark
// pushable is the same as sqlite's. This protects Task 16's parity tests.
func TestPlanQuery_IsPushableParityWithSqlite(t *testing.T) {
	// These must all be pushable (mirror sqlite's isPushable).
	pushable := []spi.FilterOp{
		spi.FilterEq, spi.FilterNe,
		spi.FilterGt, spi.FilterLt, spi.FilterGte, spi.FilterLte,
		spi.FilterContains, spi.FilterStartsWith, spi.FilterEndsWith,
		spi.FilterLike,
		spi.FilterIsNull, spi.FilterNotNull,
		spi.FilterBetween,
	}
	for _, op := range pushable {
		if !isPushable(op) {
			t.Errorf("op %s should be pushable", op)
		}
	}
	// These must NOT be pushable.
	notPushable := []spi.FilterOp{
		spi.FilterMatchesRegex,
		spi.FilterIEq, spi.FilterINe,
		spi.FilterIContains, spi.FilterINotContains,
		spi.FilterIStartsWith, spi.FilterINotStartsWith,
		spi.FilterIEndsWith, spi.FilterINotEndsWith,
	}
	for _, op := range notPushable {
		if isPushable(op) {
			t.Errorf("op %s should NOT be pushable", op)
		}
	}
}

// TestFieldExpr_MetaCanonicalMapping asserts fieldExpr resolves canonical
// SourceMeta lifecycle-filter paths through the same metaJSONKey map
// orderByFieldExpr uses for ORDER BY, and special-cases "id" to the
// entity_id column (not present in metaJSONKey).
func TestFieldExpr_MetaCanonicalMapping(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"creationDate", "creationDate", "doc->'_meta'->>'creation_date'"},
		{"lastUpdateTime", "lastUpdateTime", "doc->'_meta'->>'last_modified_date'"},
		{"transitionForLatestSave", "transitionForLatestSave", "doc->'_meta'->>'transition'"},
		{"transactionId", "transactionId", "doc->'_meta'->>'transaction_id'"},
		{"id", "id", "entity_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fieldExpr(spi.Filter{Source: spi.SourceMeta, Path: tc.path})
			if got != tc.want {
				t.Errorf("fieldExpr(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestPlan_TemporalMetaEmitsEpochMillis asserts a CoerceTemporal meta leaf
// routes through cyoda_epoch_millis on the canonically-mapped JSONB key, and
// binds a Go-precomputed int64 epoch-ms operand (not the raw RFC3339 string).
func TestPlan_TemporalMetaEmitsEpochMillis(t *testing.T) {
	f := spi.Filter{Op: spi.FilterGt, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	plan := planQuery(f)
	if !strings.Contains(plan.where, "cyoda_epoch_millis(doc->'_meta'->>'creation_date')") {
		t.Errorf("where = %q", plan.where)
	}
	wantWhere := "(cyoda_epoch_millis(doc->'_meta'->>'creation_date') IS NOT NULL AND cyoda_epoch_millis(doc->'_meta'->>'creation_date') > $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != int64(1609459200000) {
		t.Errorf("args = %v, want [1609459200000]", plan.args)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil for pushable temporal op, got %+v", plan.postFilter)
	}
}

// TestPlan_TemporalMetaNE asserts the NE 3VL form (IS NULL OR != ) is
// preserved for temporal leaves, mirroring the non-temporal NE shape.
func TestPlan_TemporalMetaNE(t *testing.T) {
	f := spi.Filter{Op: spi.FilterNe, Source: spi.SourceMeta, Path: "lastUpdateTime", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	plan := planQuery(f)
	wantWhere := "(cyoda_epoch_millis(doc->'_meta'->>'last_modified_date') IS NULL OR cyoda_epoch_millis(doc->'_meta'->>'last_modified_date') != $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != int64(1609459200000) {
		t.Errorf("args = %v, want [1609459200000]", plan.args)
	}
}

// TestPlan_TemporalMetaBetween asserts BETWEEN binds two Go-precomputed
// int64 epoch-ms operands from f.Values.
func TestPlan_TemporalMetaBetween(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: []any{"2021-01-01T00:00:00Z", "2021-06-01T14:00:00+02:00"},
	}
	plan := planQuery(f)
	wantWhere := "(cyoda_epoch_millis(doc->'_meta'->>'creation_date') IS NOT NULL AND cyoda_epoch_millis(doc->'_meta'->>'creation_date') BETWEEN $1 AND $2)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != int64(1609459200000) || plan.args[1] != int64(1622548800000) {
		t.Errorf("args = %v, want [1609459200000 1622548800000]", plan.args)
	}
}

// TestPlan_TemporalData covers a SourceData temporal leaf (non-meta path)
// to confirm CoerceTemporal routing is independent of Source.
func TestPlan_TemporalData(t *testing.T) {
	f := spi.Filter{Op: spi.FilterLte, Source: spi.SourceData, Path: "occurredAt", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	plan := planQuery(f)
	wantWhere := "(cyoda_epoch_millis(doc->>'occurredAt') IS NOT NULL AND cyoda_epoch_millis(doc->>'occurredAt') <= $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

// TestPlan_TemporalIsNull asserts that a CoerceTemporal meta leaf with
// FilterIsNull/FilterNotNull emits a plain null-check on the raw field
// expression (doc->'_meta'->>'creation_date') — NOT the cyoda_epoch_millis(...)
// wrapped form and NOT the "col = $1" / "= 0" nonsense that sqlOpForTemporal's
// unconditional "default: return \"=\"" previously produced for an op it
// doesn't recognize. Presence checks are coercion-independent: they must be
// handled before the CoerceTemporal routing, mirroring spi.evalLeafFilter's
// ordering.
func TestPlan_TemporalIsNull(t *testing.T) {
	f := spi.Filter{Op: spi.FilterIsNull, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal}
	plan := planQuery(f)
	wantWhere := "doc->'_meta'->>'creation_date' IS NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want []", plan.args)
	}
	if strings.Contains(plan.where, "cyoda_epoch_millis") {
		t.Errorf("where must not route through cyoda_epoch_millis for a presence check: %s", plan.where)
	}
	if strings.Contains(plan.where, "= $1") || strings.Contains(plan.where, "= 0") {
		t.Errorf("where must not be the nonsense equality fallback: predicate was silently corrupted: %s", plan.where)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil — IsNull must remain pushable, just correct: %+v", plan.postFilter)
	}
	if !isPushable(spi.FilterIsNull) {
		t.Errorf("FilterIsNull must remain pushable — the fix must push the CORRECT SQL, not fall back to residual")
	}
}

func TestPlan_TemporalNotNull(t *testing.T) {
	f := spi.Filter{Op: spi.FilterNotNull, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal}
	plan := planQuery(f)
	wantWhere := "doc->'_meta'->>'creation_date' IS NOT NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want []", plan.args)
	}
	if strings.Contains(plan.where, "cyoda_epoch_millis") {
		t.Errorf("where must not route through cyoda_epoch_millis for a presence check: %s", plan.where)
	}
	if plan.postFilter != nil {
		t.Errorf("postFilter should be nil — NotNull must remain pushable, just correct: %+v", plan.postFilter)
	}
	if !isPushable(spi.FilterNotNull) {
		t.Errorf("FilterNotNull must remain pushable — the fix must push the CORRECT SQL, not fall back to residual")
	}
}
