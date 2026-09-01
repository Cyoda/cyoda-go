package postgres

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestPlanQuery_EqSourceData(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "city",
		Source:   spi.SourceData,
		Value:    "Berlin",
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(doc->>'city' IS NOT NULL AND doc->>'city' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "Berlin" {
		t.Errorf("args = %v, want [Berlin]", plan.args)
	}
	// SOUND SUPERSET: Eq is not EXACT (only IsNull/NotNull are), so the kernel
	// re-checks — the FULL filter is installed as postFilter.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterEq {
		t.Errorf("postFilter should be the full Eq filter, got %+v", plan.postFilter)
	}
}

func TestPlanQuery_NeSourceData(t *testing.T) {
	// Ne is NON-pushable (SQL "!=" under-selects under float8/text collision).
	// It becomes residual-only: no WHERE fragment, kernel-evaluated.
	f := spi.Filter{
		Op:       spi.FilterNe,
		Path:     "status",
		Source:   spi.SourceData,
		Value:    "CLOSED",
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("where should be empty for non-pushable Ne, got %s", plan.where)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want [] (Ne is not pushed)", plan.args)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterNe {
		t.Fatalf("postFilter should be the Ne residual, got %+v", plan.postFilter)
	}
}

func TestPlanQuery_ComparisonOps_String(t *testing.T) {
	// String values use plain text comparison; Gt/Lt are RELAXED to >=/<=
	// (SOUND SUPERSET).
	tests := []struct {
		name  string
		op    spi.FilterOp
		sqlOp string
	}{
		{"gt", spi.FilterGt, ">="},
		{"lt", spi.FilterLt, "<="},
		{"gte", spi.FilterGte, ">="},
		{"lte", spi.FilterLte, "<="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := spi.Filter{
				Op:       tt.op,
				Path:     "name",
				Source:   spi.SourceData,
				Value:    "M",
				Declared: []spi.DataType{spi.String},
			}
			plan, err := planQuery(f)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
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
		{"gt", spi.FilterGt, ">="},
		{"lt", spi.FilterLt, "<="},
		{"gte", spi.FilterGte, ">="},
		{"lte", spi.FilterLte, "<="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := spi.Filter{
				Op:       tt.op,
				Path:     "age",
				Source:   spi.SourceData,
				Value:    float64(25),
				Declared: []spi.DataType{spi.Double},
			}
			plan, err := planQuery(f)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
			want := "(cyoda_try_float8(doc->>'age') IS NOT NULL AND cyoda_try_float8(doc->>'age') " + tt.sqlOp + " $1::float8)"
			if plan.where != want {
				t.Errorf("where:\n  got  %s\n  want %s", plan.where, want)
			}
			if len(plan.args) != 1 || plan.args[0] != float64(25) {
				t.Errorf("args = %v, want [25]", plan.args)
			}
			// SOUND SUPERSET → full-filter kernel re-check.
			if plan.postFilter == nil || plan.postFilter.Op != tt.op {
				t.Errorf("postFilter should be the full filter (op %s), got %+v", tt.op, plan.postFilter)
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "substr(doc->>'name', 1, length($1)) = $2"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != "Al" || plan.args[1] != "Al" {
		t.Errorf("args = %v, want [Al Al]", plan.args)
	}
}

func TestPlanQuery_EndsWith(t *testing.T) {
	// postgres uses right(col, char_length($N)) = $N, NOT sqlite's
	// substr(col, -length($N)) idiom — postgres substr's negative-start
	// semantics don't mean "last N characters" (see the fix's doc comment
	// on leafToSQL, case spi.FilterEndsWith, and the now-passing
	// TestPostgresPushdownSoundness_EndsWithUnderSelects_KNOWNBUG).
	f := spi.Filter{
		Op:     spi.FilterEndsWith,
		Path:   "email",
		Source: spi.SourceData,
		Value:  ".com",
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "right(doc->>'email', char_length($1)) = $2"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != ".com" || plan.args[1] != ".com" {
		t.Errorf("args = %v, want [.com .com]", plan.args)
	}
}

func TestPlanQuery_Like(t *testing.T) {
	// Like is NOT pushable (see isPushable's doc comment): SQL LIKE's
	// wildcards don't line up with Cloud's LIKE grammar, so pushing it
	// under-selects real wildcard patterns. It is residual-only — no WHERE
	// fragment, kernel-evaluated. Mirrors sqlite's TestPlanQuery_Like.
	f := spi.Filter{
		Op:     spi.FilterLike,
		Path:   "desc",
		Source: spi.SourceData,
		Value:  "foo%bar_baz\\qux",
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("where should be empty for non-pushable Like, got %s", plan.where)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want [] (Like is not pushed)", plan.args)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterLike {
		t.Fatalf("postFilter should be the Like residual, got %+v", plan.postFilter)
	}
}

func TestPlanQuery_IsNull(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterIsNull,
		Path:   "address",
		Source: spi.SourceData,
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
		Op:       spi.FilterBetween,
		Path:     "name",
		Source:   spi.SourceData,
		Values:   []any{"A", "Z"},
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
		Op:       spi.FilterBetween,
		Path:     "score",
		Source:   spi.SourceData,
		Values:   []any{float64(10), float64(20)},
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
		Op:       spi.FilterEq,
		Path:     "state",
		Source:   spi.SourceMeta,
		Value:    "ACTIVE",
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
		Op:       spi.FilterEq,
		Path:     "entity_id",
		Source:   spi.SourceMeta,
		Value:    "abc-123",
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(entity_id IS NOT NULL AND entity_id = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

func TestPlanQuery_NestedPath(t *testing.T) {
	// Dotted paths map to chained -> followed by ->> on the leaf.
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "parent.child",
		Source:   spi.SourceData,
		Value:    "x",
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(doc->'parent'->>'child' IS NOT NULL AND doc->'parent'->>'child' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

func TestPlanQuery_NestedPath_TwoLevels(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "a.b.c",
		Source:   spi.SourceData,
		Value:    "x",
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
			plan, err := planQuery(f)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
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
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
			{Op: spi.FilterEq, Path: "country", Source: spi.SourceData, Value: "DE", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}

	wantWhere := "((doc->>'city' IS NOT NULL AND doc->>'city' = $1)) AND " +
		"((doc->>'country' IS NOT NULL AND doc->>'country' = $2))"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 {
		t.Errorf("args count = %d, want 2", len(plan.args))
	}

	// Not exact (has a residual) → the FULL filter is re-checked by the kernel.
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil")
	}
	if plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter.Op = %s, want and (full filter re-check)", plan.postFilter.Op)
	}
}

func TestPlanQuery_GreedyAND_AllPushable(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterEq, Path: "country", Source: spi.SourceData, Value: "DE", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// All children pushable but none EXACT → full-filter kernel re-check.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter should be the full AND filter, got %+v", plan.postFilter)
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Munich", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// All OR children pushable but not EXACT → full-filter kernel re-check.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterOr {
		t.Errorf("postFilter should be the full OR filter, got %+v", plan.postFilter)
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
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "x", Declared: []spi.DataType{spi.String}},
					{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
				},
			},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// Fully pushable but no leaf is EXACT → full-filter kernel re-check.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter should be the full AND filter, got %+v", plan.postFilter)
	}
	if plan.where == "" {
		t.Error("where should not be empty")
	}
}

func TestPlanQuery_NestedANDWithPartialOR(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "x", Declared: []spi.DataType{spi.String}},
					{Op: spi.FilterMatchesRegex, Path: "b", Source: spi.SourceData, Value: "^y"},
				},
			},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(doc->>'city' IS NOT NULL AND doc->>'city' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	// Residual present (the partial OR) → full filter re-checked by the kernel.
	if plan.postFilter == nil {
		t.Fatal("postFilter should be non-nil")
	}
	if plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter.Op = %s, want and (full filter re-check)", plan.postFilter.Op)
	}
}

func TestPlanQuery_EmptyFilter(t *testing.T) {
	f := spi.Filter{}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
			{Op: spi.FilterEq, Path: "name", Source: spi.SourceData, Value: "Alice", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(doc->>'name' IS NOT NULL AND doc->>'name' = $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	// Single pushable Eq child, not EXACT → the FULL filter (the AND wrapper) is re-checked.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter should be the full AND filter, got %+v", plan.postFilter)
	}
}

// C1/M4 — a malformed (non-2-element) BETWEEN value now fails planQuery
// outright: spi.Prepare rejects a malformed range arity with
// ErrUnevaluableLeaf, and planQuery propagates it rather than computing a
// fail-closed exclude predicate. Validation also rejects this upstream (see
// internal/domain/search/operators.go validateBetweenArity), so this is
// defense-in-depth for any Filter constructed directly (bypassing the
// domain validator) — the request is rejected, never silently answered as
// an empty page, and never a panic.
func TestPlanQuery_BetweenInsufficientValues(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "val",
		Source: spi.SourceData,
		Values: []any{float64(10)},
	}
	_, err := planQuery(f)
	if err == nil {
		t.Fatal("planQuery must fail on a malformed BETWEEN arity, not silently plan an exclude predicate")
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
	}
}

// TestPlan_TemporalBetween_NilValues_DoesNotPanic is the C1 regression test:
// a CoerceTemporal BETWEEN leaf with Values=nil (the shape produced by
// betweenValues for a malformed BETWEEN operand, before the fix landed) must
// not panic indexing f.Values[0]/[1] — it now fails planQuery outright via
// spi.Prepare's ErrUnevaluableLeaf (malformed range arity), never the
// sqlite/memory-diverging match-all and never a panic.
func TestPlan_TemporalBetween_NilValues_DoesNotPanic(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: nil,
	}
	_, err := planQuery(f)
	if err == nil {
		t.Fatal("planQuery must fail on a malformed temporal BETWEEN with no values")
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
	}
}

// TestPlan_TemporalBetween_OneValue_DoesNotPanic covers the 1-element-array
// shape of the same malformed condition.
func TestPlan_TemporalBetween_OneValue_DoesNotPanic(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: []any{"2021-01-01T00:00:00Z"},
	}
	_, err := planQuery(f)
	if err == nil {
		t.Fatal("planQuery must fail on a malformed temporal BETWEEN with one value")
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
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
			{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "1", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "2", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterEq, Path: "c", Source: spi.SourceData, Value: "3", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
			{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
					{Op: spi.FilterEq, Path: "a", Source: spi.SourceData, Value: "x", Declared: []spi.DataType{spi.String}},
					{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
				},
			},
			{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					{Op: spi.FilterGt, Path: "c", Source: spi.SourceData, Value: float64(1), Declared: []spi.DataType{spi.Double}},
					{Op: spi.FilterLt, Path: "d", Source: spi.SourceData, Value: float64(100), Declared: []spi.DataType{spi.Double}},
				},
			},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// Fully pushable tree, but none of its leaves is EXACT → full re-check.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter should be the full AND filter, got %+v", plan.postFilter)
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "doc->'_meta'->>'state' IS NULL"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

// TestPlanQuery_IsPushableParityWithSqlite asserts the set of ops we mark
// pushable is the same as sqlite's. This protects Task 16's parity tests.
func TestPlanQuery_IsPushableParityWithSqlite(t *testing.T) {
	// These must all be pushable (mirror sqlite's isPushable). Ne and Like are
	// deliberately excluded (residual-only); BetweenInclusive is included.
	pushable := []spi.FilterOp{
		spi.FilterEq,
		spi.FilterGt, spi.FilterLt, spi.FilterGte, spi.FilterLte,
		spi.FilterContains, spi.FilterStartsWith, spi.FilterEndsWith,
		spi.FilterIsNull, spi.FilterNotNull,
		spi.FilterBetween, spi.FilterBetweenInclusive,
	}
	for _, op := range pushable {
		if !isPushable(op) {
			t.Errorf("op %s should be pushable", op)
		}
	}
	// These must NOT be pushable. Ne under-selects in SQL (float8/text collision),
	// so it is residual-only. Like's SQL-wildcard grammar doesn't line up with
	// Cloud's LIKE grammar, so it is residual-only too (see isPushable's doc
	// comment).
	notPushable := []spi.FilterOp{
		spi.FilterNe,
		spi.FilterLike,
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
	f := spi.Filter{Op: spi.FilterGt, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z", Declared: []spi.DataType{spi.ZonedDateTime}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if !strings.Contains(plan.where, "cyoda_epoch_millis(doc->'_meta'->>'creation_date')") {
		t.Errorf("where = %q", plan.where)
	}
	// SOUND SUPERSET: temporal Gt is relaxed to >=.
	wantWhere := "(cyoda_epoch_millis(doc->'_meta'->>'creation_date') IS NOT NULL AND cyoda_epoch_millis(doc->'_meta'->>'creation_date') >= $1)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != int64(1609459200000) {
		t.Errorf("args = %v, want [1609459200000]", plan.args)
	}
	// Temporal compare is a SOUND SUPERSET → full-filter kernel re-check.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterGt {
		t.Errorf("postFilter should be the full Gt filter, got %+v", plan.postFilter)
	}
}

// TestPlan_TemporalMetaNE asserts the NE 3VL form (IS NULL OR != ) is
// preserved for temporal leaves, mirroring the non-temporal NE shape.
func TestPlan_TemporalMetaNE(t *testing.T) {
	// Ne is non-pushable (isPushable is coercion-blind) → residual-only.
	f := spi.Filter{Op: spi.FilterNe, Source: spi.SourceMeta, Path: "lastUpdateTime", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z", Declared: []spi.DataType{spi.ZonedDateTime}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("temporal Ne must not be pushed, got where %q", plan.where)
	}
	if len(plan.args) != 0 {
		t.Errorf("args = %v, want [] (Ne is not pushed)", plan.args)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterNe {
		t.Fatalf("postFilter should be the Ne residual, got %+v", plan.postFilter)
	}
}

// TestPlan_TemporalMetaBetween asserts BETWEEN binds two Go-precomputed
// int64 epoch-ms operands from f.Values.
func TestPlan_TemporalMetaBetween(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: []any{"2021-01-01T00:00:00Z", "2021-06-01T14:00:00+02:00"}, Declared: []spi.DataType{spi.ZonedDateTime},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(cyoda_epoch_millis(doc->'_meta'->>'creation_date') IS NOT NULL AND cyoda_epoch_millis(doc->'_meta'->>'creation_date') BETWEEN $1 AND $2)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != int64(1609459200000) || plan.args[1] != int64(1622548800000) {
		t.Errorf("args = %v, want [1609459200000 1622548800000]", plan.args)
	}
}

// TestPlan_TemporalData asserts that a SourceData temporal COMPARISON leaf is
// NOT pushed — it is routed to the residual so the kernel
// (spi.Prepare/PreparedFilter.Match), which performs temporal-subtype
// resolution, is authoritative. The flat
// epoch-ms push (cyoda_epoch_millis) cannot reproduce the kernel's imprecise-
// floor op mutation as a sound superset, and cyoda_epoch_millis returns NULL
// for a bare ISO subtype (e.g. "2024" / "2024-09-09"), so pushing would
// under-select. Meta temporal leaves (full offset-bearing instants) remain
// pushable — see TestPlan_Temporal*. This is a deliberate leaf-level mirror
// divergence from the op-level isPushable set (identical results, not
// identical WHERE clauses).
func TestPlan_TemporalData(t *testing.T) {
	f := spi.Filter{Op: spi.FilterLte, Source: spi.SourceData, Path: "occurredAt", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z", Declared: []spi.DataType{spi.ZonedDateTime}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("data temporal comparison must not be pushed; got where=%q", plan.where)
	}
	if plan.postFilter == nil {
		t.Errorf("data temporal comparison must be routed to the residual (kernel-authoritative); postFilter is nil")
	}
	if isLeafPushable(f) {
		t.Errorf("isLeafPushable must be false for a SourceData CoerceTemporal comparison leaf")
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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

// --- json.Number operand handling (Task 6c) ---
//
// The SPI's predicate parser now decodes numeric search operands as
// json.Number (a string-kind type) instead of float64, to preserve
// precision losslessly. json.Number must still be treated as numeric by
// the query planner's pushdown routing — otherwise it misroutes to the
// lexical text-comparison branch, diverging from the memory/SPI kernel
// (which type-switches on the underlying numeric value, not the Go kind).

func TestIsNumericValue_JSONNumber(t *testing.T) {
	if !isNumericValue(json.Number("5")) {
		t.Errorf("isNumericValue(json.Number(\"5\")) = false, want true")
	}
	if !isNumericValue(json.Number("3.14")) {
		t.Errorf("isNumericValue(json.Number(\"3.14\")) = false, want true")
	}
}

func TestPlanQuery_Eq_JSONNumberOperand(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "age",
		Source:   spi.SourceData,
		Value:    json.Number("25"),
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(cyoda_try_float8(doc->>'age') IS NOT NULL AND cyoda_try_float8(doc->>'age') = $1::float8)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if strings.Contains(plan.where, "doc->>'age' = $1)") {
		t.Errorf("json.Number operand must not fall through to the lexical text-comparison branch: %s", plan.where)
	}
	if len(plan.args) != 1 || plan.args[0] != float64(25) {
		t.Errorf("args = %v, want [25.0] (bound as a float64, not the raw json.Number string)", plan.args)
	}
}

func TestPlanQuery_Ne_JSONNumberOperand(t *testing.T) {
	// Ne is non-pushable regardless of operand kind — never translated to SQL.
	f := spi.Filter{
		Op:       spi.FilterNe,
		Path:     "age",
		Source:   spi.SourceData,
		Value:    json.Number("25"),
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" || len(plan.args) != 0 {
		t.Errorf("Ne must not be pushed: where=%q args=%v", plan.where, plan.args)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterNe {
		t.Fatalf("postFilter should be the Ne residual, got %+v", plan.postFilter)
	}
}

func TestPlanQuery_Gt_JSONNumberOperand(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterGt,
		Path:     "age",
		Source:   spi.SourceData,
		Value:    json.Number("25"),
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// SOUND SUPERSET: Gt relaxed to >=.
	wantWhere := "(cyoda_try_float8(doc->>'age') IS NOT NULL AND cyoda_try_float8(doc->>'age') >= $1::float8)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != float64(25) {
		t.Errorf("args = %v, want [25.0] (bound as a float64, not the raw json.Number string)", plan.args)
	}
}

func TestPlanQuery_Between_JSONNumberOperand(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterBetween,
		Path:     "score",
		Source:   spi.SourceData,
		Values:   []any{json.Number("10"), json.Number("20")},
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(cyoda_try_float8(doc->>'score') IS NOT NULL AND cyoda_try_float8(doc->>'score') BETWEEN $1::float8 AND $2::float8)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != float64(10) || plan.args[1] != float64(20) {
		t.Errorf("args = %v, want [10.0 20.0] (bound as float64, not raw json.Number strings)", plan.args)
	}
}

// --- SQL-pushdown soundness contract (Task 11) ---

// TestSoundness_ExactFastPath asserts a plan whose pushed leaves are ALL EXACT
// (IsNull/NotNull only) keeps postFilter == nil — the SQL LIMIT/OFFSET fast
// path stays enabled because the SQL matches the kernel bit-for-bit.
func TestSoundness_ExactFastPath(t *testing.T) {
	cases := []struct {
		name string
		f    spi.Filter
	}{
		{"is_null", spi.Filter{Op: spi.FilterIsNull, Path: "a", Source: spi.SourceData}},
		{"not_null", spi.Filter{Op: spi.FilterNotNull, Path: "a", Source: spi.SourceData}},
		{"and of presence checks", spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
			{Op: spi.FilterIsNull, Path: "a", Source: spi.SourceData},
			{Op: spi.FilterNotNull, Path: "b", Source: spi.SourceData},
		}}},
		{"or of presence checks", spi.Filter{Op: spi.FilterOr, Children: []spi.Filter{
			{Op: spi.FilterIsNull, Path: "a", Source: spi.SourceData},
			{Op: spi.FilterNotNull, Path: "b", Source: spi.SourceData},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planQuery(tc.f)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
			if plan.postFilter != nil {
				t.Errorf("postFilter should be nil (fast path) for an all-EXACT plan, got %+v", plan.postFilter)
			}
			if plan.where == "" {
				t.Error("where should narrow even on the fast path")
			}
		})
	}
}

// TestSoundness_MixedPresenceAndValue asserts adding a single non-EXACT leaf
// (Eq) to a presence check disables the fast path: the whole plan is re-checked
// against the FULL filter.
func TestSoundness_MixedPresenceAndValue(t *testing.T) {
	f := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		{Op: spi.FilterNotNull, Path: "a", Source: spi.SourceData},
		{Op: spi.FilterEq, Path: "b", Source: spi.SourceData, Value: "x", Declared: []spi.DataType{spi.String}},
	}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Fatalf("a non-EXACT leaf must force a full-filter re-check, got %+v", plan.postFilter)
	}
	if plan.where == "" {
		t.Error("the EXACT + SOUND-SUPERSET leaves must still narrow the WHERE")
	}
}

// TestSoundness_BetweenInclusivePushable asserts BETWEEN_INCLUSIVE is pushable
// as a SOUND SUPERSET (inclusive SQL BETWEEN) and forces a full-filter re-check.
func TestSoundness_BetweenInclusivePushable(t *testing.T) {
	if !isPushable(spi.FilterBetweenInclusive) {
		t.Fatal("FilterBetweenInclusive must be pushable")
	}
	f := spi.Filter{Op: spi.FilterBetweenInclusive, Path: "score", Source: spi.SourceData, Values: []any{float64(10), float64(20)}, Declared: []spi.DataType{spi.Double}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(cyoda_try_float8(doc->>'score') IS NOT NULL AND cyoda_try_float8(doc->>'score') BETWEEN $1::float8 AND $2::float8)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterBetweenInclusive {
		t.Errorf("postFilter should be the full BetweenInclusive filter, got %+v", plan.postFilter)
	}
}

// TestSoundness_ExclusiveBetweenIsInclusiveSuperset asserts the exclusive kernel
// FilterBetween pushes an inclusive SQL BETWEEN (a sound superset) and re-checks.
func TestSoundness_ExclusiveBetweenIsInclusiveSuperset(t *testing.T) {
	f := spi.Filter{Op: spi.FilterBetween, Path: "score", Source: spi.SourceData, Values: []any{float64(10), float64(20)}, Declared: []spi.DataType{spi.Double}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(cyoda_try_float8(doc->>'score') IS NOT NULL AND cyoda_try_float8(doc->>'score') BETWEEN $1::float8 AND $2::float8)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s (inclusive SQL BETWEEN is a superset of the exclusive kernel)", plan.where, wantWhere)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterBetween {
		t.Errorf("postFilter should be the full Between filter so the kernel enforces the open bounds, got %+v", plan.postFilter)
	}
}

// TestSoundness_NeNonPushable asserts Ne (temporal or plain) is residual-only.
func TestSoundness_NeNonPushable(t *testing.T) {
	if isPushable(spi.FilterNe) {
		t.Fatal("FilterNe must NOT be pushable")
	}
	f := spi.Filter{Op: spi.FilterNe, Path: "creationDate", Source: spi.SourceMeta, Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z", Declared: []spi.DataType{spi.ZonedDateTime}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("temporal Ne must not be pushed, got where %q", plan.where)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterNe {
		t.Fatalf("postFilter should be the Ne residual, got %+v", plan.postFilter)
	}
}

// TestJsonbExtract_RendersSubscript checks jsonbExtractText renders a "[N]"
// hop as an INTEGER accessor (doc->'tags'->>0), never a text key
// (doc->>'tags[0]'). A text key against a JSONB array yields null, which is
// the defect this closes — see docs/cloud-parity/path-grammar.md section 9.
func TestJsonbExtract_RendersSubscript(t *testing.T) {
	cases := []struct{ path, want string }{
		{"amount", "doc->>'amount'"},
		{"obj.0", "doc->'obj'->>'0'"},
		{"tags[0]", "doc->'tags'->>0"},
		{"items[2].sku", "doc->'items'->2->>'sku'"},
		{"m[0][1]", "doc->'m'->0->>1"},
	}
	for _, tc := range cases {
		got := jsonbExtractText("doc", tc.path)
		if got != tc.want {
			t.Errorf("jsonbExtractText(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestJsonbExtract_WildcardIsRejectedNotDropped is M10: pathAccessors used to
// silently DROP a "[*]" subscript, keeping the hop's name — so
// jsonbExtractText("doc", "tags[*]") rendered doc->>'tags', a real but WRONG
// value (the whole array's text form, not "no value"). This surface is
// guarded at every legitimate caller (isLeafPushable for the WHERE clause,
// validateGroupAndAggregatePaths/validateOrderSpecs for group-by/aggregate/
// sort paths all refuse a wildcard leaf before it can reach here), so a
// defensively-reached wildcard must degrade the same safe way an unparseable
// path already does — root->>"" — never render the container as if the
// wildcard were not there.
func TestJsonbExtract_WildcardIsRejectedNotDropped(t *testing.T) {
	cases := []struct{ path, want string }{
		{"tags[*]", "doc->>''"},
		{"items[*].sku", "doc->>''"},
		{"m[0][*]", "doc->>''"},
	}
	for _, tc := range cases {
		got := jsonbExtractText("doc", tc.path)
		if got != tc.want {
			t.Errorf("jsonbExtractText(%q) = %q, want %q (safe non-match, not the container)", tc.path, got, tc.want)
		}
	}
}

// TestPlanQuery_WildcardIsResidual: a wildcard leaf has no SQL form until a
// quantifier node exists. Pushing it as a scalar comparison would drop every
// matching row, and a narrowing WHERE cannot be recovered by the residual
// re-check, so the wildcard leaf must be installed as a residual instead.
func TestPlanQuery_WildcardIsResidual(t *testing.T) {
	plan, err := planQuery(spi.Filter{
		Op: spi.FilterEq, Path: "tags[*]", Source: spi.SourceData,
		Value: "A", Declared: []spi.DataType{spi.String},
	})
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("wildcard leaf must not narrow; got WHERE %q", plan.where)
	}
	if plan.postFilter == nil {
		t.Error("wildcard leaf must be installed as a residual")
	}
}

// TestPathHasWildcard_ParseFailureIsTreatedAsWildcard pins pathHasWildcard's
// default on a path spi.ParseFilterPath cannot parse. f.Path reaching this
// function has already passed validateFilterPaths, so this is unreachable in
// practice with a well-formed caller — but the DEFAULT direction still
// matters, for exactly the reason TestPlanQuery_WildcardIsResidual states:
// treating an unparseable path as "definitely not a wildcard" would let
// isLeafPushable push it down as a scalar comparison, which for an ACTUAL
// wildcard drops every matching row with no way for the residual re-check to
// recover them. Returning true (fail closed: "might be a wildcard, don't
// push") is the safe default, matching
// .claude/rules/correctness-over-availability.md.
func TestPathHasWildcard_ParseFailureIsTreatedAsWildcard(t *testing.T) {
	for _, p := range []string{"a[", "a]", "a[-1]", "a[?(@.x)]"} {
		if !pathHasWildcard(p) {
			t.Errorf("pathHasWildcard(%q): want true (fail-closed, not pushable) on a path that fails to parse", p)
		}
	}
}

// TestPlanQuery_PositionalIsPushed: a positional subscript is a sound,
// pushable leaf and renders postgres's integer-accessor spelling.
func TestPlanQuery_PositionalIsPushed(t *testing.T) {
	plan, err := planQuery(spi.Filter{
		Op: spi.FilterEq, Path: "tags[0]", Source: spi.SourceData,
		Value: "A", Declared: []spi.DataType{spi.String},
	})
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if !strings.Contains(plan.where, "doc->'tags'->>0") {
		t.Errorf("positional leaf must push its dialect index; got WHERE %q", plan.where)
	}
}
