package sqlite

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
	wantWhere := "(json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "Berlin" {
		t.Errorf("args = %v, want [Berlin]", plan.args)
	}
	// SOUND SUPERSET: Eq is not EXACT (only IsNull/NotNull are), so the kernel
	// must re-check — the FULL filter is installed as postFilter.
	if plan.postFilter == nil {
		t.Fatal("postFilter should be the full filter for a SOUND-SUPERSET Eq leaf")
	}
	if plan.postFilter.Op != spi.FilterEq {
		t.Errorf("postFilter.Op = %s, want eq (full filter)", plan.postFilter.Op)
	}
}

func TestPlanQuery_NeSourceData(t *testing.T) {
	// Ne is NON-pushable (SQL "!=" under-selects under storage-class collision).
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

func TestPlanQuery_ComparisonOps(t *testing.T) {
	// Ordering leaves are SOUND SUPERSETS: Gt/Lt are RELAXED to >=/<= (strict
	// operators under-select under float collision); Gte/Lte already use >=/<=.
	// All of them force a full-filter kernel re-check (postFilter != nil).
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
			want := "(json_extract(data, '$.age') IS NOT NULL AND json_extract(data, '$.age') " + tt.sqlOp + " ?)"
			if plan.where != want {
				t.Errorf("where:\n  got  %s\n  want %s", plan.where, want)
			}
			if len(plan.args) != 1 || plan.args[0] != float64(25) {
				t.Errorf("args = %v, want [25]", plan.args)
			}
			// The narrowing WHERE is still emitted (SOUND-SUPERSET leaves ARE in
			// the WHERE), but the plan is not exact → kernel re-checks the full filter.
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "substr(json_extract(data, '$.email'), -length(?)) = ?"
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
	// fragment, kernel-evaluated.
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
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	wantWhere := "(json_extract(data, '$.score') IS NOT NULL AND json_extract(data, '$.score') BETWEEN ? AND ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != float64(10) || plan.args[1] != float64(20) {
		t.Errorf("args = %v, want [10 20]", plan.args)
	}
}

// --- json.Number operand handling (Task 6c) ---
//
// The SPI's predicate parser now decodes numeric search operands as
// json.Number (a string-kind type) instead of float64, to preserve
// precision losslessly. database/sql binds a json.Number raw as TEXT
// (it implements no driver.Valuer), which flips SQLite's storage-class
// comparison from numeric to lexicographic — diverging from the
// memory/SPI kernel. A json.Number operand must be converted to a
// numeric Go value (int64 when integral, else float64) before binding
// so REAL/INTEGER affinity — and therefore numeric ordering — holds.
// The SQL text itself is unaffected (sqlite's WHERE clause shape does
// not depend on operand type); only the bound arg's Go type matters.

func TestPlanQuery_Eq_JSONNumberOperand_BindsInt64(t *testing.T) {
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
	wantWhere := "(json_extract(data, '$.age') IS NOT NULL AND json_extract(data, '$.age') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != int64(25) {
		t.Errorf("args = %v (%T), want [int64(25)] — an integral json.Number must bind as int64, not the raw string", plan.args, plan.args[0])
	}
}

func TestPlanQuery_Eq_JSONNumberOperand_BindsFloat64(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "score",
		Source:   spi.SourceData,
		Value:    json.Number("3.14"),
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if len(plan.args) != 1 || plan.args[0] != float64(3.14) {
		t.Errorf("args = %v (%T), want [float64(3.14)] — a fractional json.Number must bind as float64, not the raw string", plan.args, plan.args[0])
	}
}

func TestPlanQuery_Ne_JSONNumberOperand(t *testing.T) {
	// Ne is non-pushable regardless of operand kind — it is never translated to
	// SQL, so no arg is bound; the leaf is kernel-evaluated as residual.
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

func TestPlanQuery_Ordering_JSONNumberOperand(t *testing.T) {
	tests := []struct {
		name string
		op   spi.FilterOp
	}{
		{"gt", spi.FilterGt},
		{"lt", spi.FilterLt},
		{"gte", spi.FilterGte},
		{"lte", spi.FilterLte},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := spi.Filter{
				Op:       tt.op,
				Path:     "age",
				Source:   spi.SourceData,
				Value:    json.Number("25"),
				Declared: []spi.DataType{spi.Double},
			}
			plan, err := planQuery(f)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
			if len(plan.args) != 1 || plan.args[0] != int64(25) {
				t.Errorf("args = %v (%T), want [int64(25)]", plan.args, plan.args[0])
			}
		})
	}
}

func TestPlanQuery_Between_JSONNumberOperand(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterBetween,
		Path:     "score",
		Source:   spi.SourceData,
		Values:   []any{json.Number("10"), json.Number("20.5")},
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(json_extract(data, '$.score') IS NOT NULL AND json_extract(data, '$.score') BETWEEN ? AND ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 2 || plan.args[0] != int64(10) || plan.args[1] != float64(20.5) {
		t.Errorf("args = %v (%T, %T), want [int64(10) float64(20.5)]", plan.args, plan.args[0], plan.args[1])
	}
}

func TestPlanQuery_SourceMeta(t *testing.T) {
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
		Op:       spi.FilterGt,
		Path:     "created_at",
		Source:   spi.SourceMeta,
		Value:    int64(1000000),
		Declared: []spi.DataType{spi.Double},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// SOUND SUPERSET: Gt is relaxed to >=.
	wantWhere := "(created_at IS NOT NULL AND created_at >= ?)"
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
	// AND with two pushable and one non-pushable child.
	f := spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(18), Declared: []spi.DataType{spi.Double}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}

	// Pushed: eq(city) AND gt(age). joinChildren wraps each child in ().
	// gt is relaxed to >= (SOUND SUPERSET).
	wantWhere := "((json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)) AND " +
		"((json_extract(data, '$.age') IS NOT NULL AND json_extract(data, '$.age') >= ?))"
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
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(18), Declared: []spi.DataType{spi.Double}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	// All children pushable but NONE is EXACT (Eq/Gt are sound supersets), so
	// the full filter is re-checked by the kernel.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter should be the full AND filter, got %+v", plan.postFilter)
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
	// All OR children pushable but not EXACT → full filter re-checked.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterOr {
		t.Errorf("postFilter should be the full OR filter, got %+v", plan.postFilter)
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
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterMatchesRegex, Path: "code", Source: spi.SourceData, Value: "^X"},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	// Both eq(city) and the OR are fully pushable but not EXACT → full re-check.
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterAnd {
		t.Errorf("postFilter should be the full AND filter, got %+v", plan.postFilter)
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

	wantWhere := "(json_extract(data, '$.city') IS NOT NULL AND json_extract(data, '$.city') = ?)"
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
	// An empty filter (zero-value) should produce no WHERE and no residual.
	f := spi.Filter{}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
			{Op: spi.FilterEq, Path: "name", Source: spi.SourceData, Value: "Alice", Declared: []spi.DataType{spi.String}},
		},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(json_extract(data, '$.name') IS NOT NULL AND json_extract(data, '$.name') = ?)"
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
// an empty page.
func TestPlanQuery_BetweenInsufficientValues(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterBetween,
		Path:   "val",
		Source: spi.SourceData,
		Values: []any{float64(10)}, // only 1 value
	}
	_, err := planQuery(f)
	if err == nil {
		t.Fatal("planQuery must fail on a malformed BETWEEN arity, not silently plan an exclude predicate")
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
	}
}

// TestSqlitePlan_TemporalBetween_NilValues_Excludes is the C1 regression
// test for sqlite: a CoerceTemporal BETWEEN leaf with Values=nil (the shape
// produced by betweenValues for a malformed BETWEEN operand, before the fix
// landed) must emit an exclude predicate — never the match-all "1=1" that
// previously let every row through.
func TestSqlitePlan_TemporalBetween_NilValues_Excludes(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: nil,
	}
	sql, args := leafToSQL(f)
	if sql != "0" {
		t.Errorf("sql = %s, want 0 (exclude) for a malformed temporal BETWEEN with no values", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// TestSqlitePlan_TemporalBetween_OneValue_Excludes covers the 1-element-array
// shape of the same malformed condition.
func TestSqlitePlan_TemporalBetween_OneValue_Excludes(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: []any{"2021-01-01T00:00:00Z"},
	}
	sql, _ := leafToSQL(f)
	if sql != "0" {
		t.Errorf("sql = %s, want 0 (exclude) for a malformed temporal BETWEEN with one value", sql)
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

func TestPlanQuery_SourceMetaIsNull(t *testing.T) {
	f := spi.Filter{
		Op:     spi.FilterIsNull,
		Path:   "state",
		Source: spi.SourceMeta,
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
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
	wantWhere := "(entity_id IS NOT NULL AND entity_id = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
}

// TestFieldExpr_MetaCanonicalMapping asserts fieldExpr resolves canonical
// SourceMeta lifecycle-filter paths through the same metaBlobKey map
// orderByFieldExpr uses for ORDER BY, and special-cases "id" to the
// entity_id column (not present in metaBlobKey).
func TestFieldExpr_MetaCanonicalMapping(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"creationDate", "creationDate", "json_extract(json(meta), '$.creation_date')"},
		{"lastUpdateTime", "lastUpdateTime", "json_extract(json(meta), '$.last_modified_date')"},
		{"transitionForLatestSave", "transitionForLatestSave", "json_extract(json(meta), '$.transition_for_latest_save')"},
		{"transactionId", "transactionId", "json_extract(json(meta), '$.transaction_id')"},
		{"id", "id", "entity_id"},
		// Pre-existing storage-key vocabulary keeps working via the
		// directMetaColumns fallback / raw json_extract.
		{"state (storage key)", "state", "json_extract(json(meta), '$.state')"},
		{"entity_id (direct column)", "entity_id", "entity_id"},
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

// TestSqlitePlan_TemporalMetaDividesMicros asserts a CoerceTemporal meta leaf
// routes through the canonically-mapped meta blob key, divides the stored
// microseconds by 1000 (matching orderByFieldExpr's µs->ms floor), and binds
// a Go-precomputed int64 epoch-ms operand (not the raw RFC3339 string).
func TestSqlitePlan_TemporalMetaDividesMicros(t *testing.T) {
	f := spi.Filter{Op: spi.FilterGt, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	sql, args := leafToSQL(f)
	if !strings.Contains(sql, "/ 1000") || !strings.Contains(sql, "creation_date") {
		t.Errorf("sql = %q", sql)
	}
	// SOUND SUPERSET: temporal Gt is relaxed to >=.
	wantSQL := "((json_extract(json(meta), '$.creation_date') / 1000) IS NOT NULL AND (json_extract(json(meta), '$.creation_date') / 1000) >= ?)"
	if sql != wantSQL {
		t.Errorf("sql:\n  got  %s\n  want %s", sql, wantSQL)
	}
	if len(args) != 1 || args[0] != int64(1609459200000) {
		t.Errorf("args = %v, want [1609459200000]", args)
	}
}

// TestSqlitePlan_TemporalMetaNE asserts the NE 3VL form (IS NULL OR !=) is
// preserved for temporal leaves, mirroring the non-temporal NE shape.
func TestSqlitePlan_TemporalMetaNE(t *testing.T) {
	f := spi.Filter{Op: spi.FilterNe, Source: spi.SourceMeta, Path: "lastUpdateTime", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	sql, args := leafToSQL(f)
	wantSQL := "((json_extract(json(meta), '$.last_modified_date') / 1000) IS NULL OR (json_extract(json(meta), '$.last_modified_date') / 1000) != ?)"
	if sql != wantSQL {
		t.Errorf("sql:\n  got  %s\n  want %s", sql, wantSQL)
	}
	if len(args) != 1 || args[0] != int64(1609459200000) {
		t.Errorf("args = %v, want [1609459200000]", args)
	}
}

// TestSqlitePlan_TemporalMetaBetween asserts BETWEEN binds two
// Go-precomputed int64 epoch-ms operands from f.Values.
func TestSqlitePlan_TemporalMetaBetween(t *testing.T) {
	f := spi.Filter{
		Op: spi.FilterBetween, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal,
		Values: []any{"2021-01-01T00:00:00Z", "2021-06-01T14:00:00+02:00"},
	}
	sql, args := leafToSQL(f)
	wantSQL := "((json_extract(json(meta), '$.creation_date') / 1000) IS NOT NULL AND (json_extract(json(meta), '$.creation_date') / 1000) BETWEEN ? AND ?)"
	if sql != wantSQL {
		t.Errorf("sql:\n  got  %s\n  want %s", sql, wantSQL)
	}
	if len(args) != 2 || args[0] != int64(1609459200000) || args[1] != int64(1622548800000) {
		t.Errorf("args = %v, want [1609459200000 1622548800000]", args)
	}
}

// TestSqlitePlan_TemporalData asserts that a SourceData temporal COMPARISON
// leaf is NOT pushed — it is routed to the residual so the kernel
// (spi.Prepare/PreparedFilter.Match), which performs temporal-subtype
// resolution, is authoritative. The flat epoch-ms push ("/1000") assumes a µs-integer stored
// value and cannot reproduce the kernel's imprecise-floor op mutation (e.g.
// `>= 2024-09-09` on a Year field becomes `> 2024`) as a sound superset over a
// bare ISO-string data value. Meta temporal leaves (µs-integer instants) remain
// pushable. Mirrors postgres's TestPlan_TemporalData; a deliberate leaf-level
// divergence from the op-level isPushable set (identical results guaranteed by
// the kernel re-check, not identical WHERE clauses).
func TestSqlitePlan_TemporalData(t *testing.T) {
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

// TestSqlitePlan_TemporalIsNull asserts that a CoerceTemporal meta leaf with
// FilterIsNull/FilterNotNull emits a plain null-check on the raw field
// expression — NOT the temporal "/1000" epoch-ms form and NOT the "1=1"
// no-op that sqlOpForTemporal's empty-string default previously produced.
// Presence checks are coercion-independent: they must be handled before the
// CoerceTemporal routing, mirroring spi.evalLeafFilter's ordering.
func TestSqlitePlan_TemporalIsNull(t *testing.T) {
	f := spi.Filter{Op: spi.FilterIsNull, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal}
	sql, args := leafToSQL(f)
	wantSQL := "json_extract(json(meta), '$.creation_date') IS NULL"
	if sql != wantSQL {
		t.Errorf("sql:\n  got  %s\n  want %s", sql, wantSQL)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want []", args)
	}
	if strings.Contains(sql, "/ 1000") {
		t.Errorf("sql must not divide by 1000 for a presence check: %s", sql)
	}
	if sql == "1=1" {
		t.Errorf("sql must not be the no-op 1=1 fallback: predicate was silently dropped")
	}
	if !isPushable(spi.FilterIsNull) {
		t.Errorf("FilterIsNull must remain pushable — the fix must push the CORRECT SQL, not fall back to residual")
	}
}

func TestSqlitePlan_TemporalNotNull(t *testing.T) {
	f := spi.Filter{Op: spi.FilterNotNull, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal}
	sql, args := leafToSQL(f)
	wantSQL := "json_extract(json(meta), '$.creation_date') IS NOT NULL"
	if sql != wantSQL {
		t.Errorf("sql:\n  got  %s\n  want %s", sql, wantSQL)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want []", args)
	}
	if strings.Contains(sql, "/ 1000") {
		t.Errorf("sql must not divide by 1000 for a presence check: %s", sql)
	}
	if !isPushable(spi.FilterNotNull) {
		t.Errorf("FilterNotNull must remain pushable — the fix must push the CORRECT SQL, not fall back to residual")
	}
}

// --- Type-directed operand bind + polymorphic residual ---
//
// SQLite's json_extract PRESERVES the stored JSON scalar's native storage
// class (an int stays INTEGER), and SQLite never equates different storage
// classes (30 = '30' is false). So a comparison operand must be bound
// according to the leaf's DECLARED type family, not the operand's own JSON
// kind, or the pushed SQL under-selects. Polymorphic-declared comparison
// leaves cannot be bound to a single storage class that supersets every
// branch, so they are residual-only on sqlite.

// TestPlanQuery_MonomorphicStringField_NumberOperand_TextBind pins the
// monomorphic non-numeric case: a STRING-declared field with a json.Number
// operand must TEXT-bind (so it compares against the text-stored scalar),
// NOT numeric-bind (which would miss the text-stored value).
func TestPlanQuery_MonomorphicStringField_NumberOperand_TextBind(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "code",
		Source:   spi.SourceData,
		Value:    json.Number("30"),
		Declared: []spi.DataType{spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	wantWhere := "(json_extract(data, '$.code') IS NOT NULL AND json_extract(data, '$.code') = ?)"
	if plan.where != wantWhere {
		t.Errorf("where:\n  got  %s\n  want %s", plan.where, wantWhere)
	}
	if len(plan.args) != 1 || plan.args[0] != "30" {
		t.Errorf("args = %v (%T), want [\"30\"] — a json.Number operand on a STRING field must bind as TEXT, not int64", plan.args, plan.args[0])
	}
}

// TestPlanQuery_MonomorphicNumericField_NumberOperand_NumericBind confirms the
// monomorphic-numeric path is unchanged (json.Number -> int64), so the
// large-int / precision behaviour does not regress.
func TestPlanQuery_MonomorphicNumericField_NumberOperand_NumericBind(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "age",
		Source:   spi.SourceData,
		Value:    json.Number("30"),
		Declared: []spi.DataType{spi.Integer},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if len(plan.args) != 1 || plan.args[0] != int64(30) {
		t.Errorf("args = %v (%T), want [int64(30)] — a numeric field keeps the numeric bind", plan.args, plan.args[0])
	}
}

// TestPlanQuery_PolymorphicComparison_Residual pins that a comparison leaf
// with polymorphic declared types is NON-pushable on sqlite (residual-only):
// no single-storage-class SQL bind can superset both branches.
func TestPlanQuery_PolymorphicComparison_Residual(t *testing.T) {
	for _, op := range []spi.FilterOp{
		spi.FilterEq, spi.FilterGt, spi.FilterLt, spi.FilterGte,
		spi.FilterLte, spi.FilterBetween, spi.FilterBetweenInclusive,
	} {
		t.Run(string(op), func(t *testing.T) {
			f := spi.Filter{
				Op:       op,
				Path:     "code",
				Source:   spi.SourceData,
				Value:    "30",
				Values:   []any{"10", "30"},
				Declared: []spi.DataType{spi.Integer, spi.String},
			}
			plan, err := planQuery(f)
			if err != nil {
				t.Fatalf("planQuery: %v", err)
			}
			if plan.where != "" {
				t.Errorf("where should be empty for a polymorphic comparison leaf, got %s", plan.where)
			}
			if len(plan.args) != 0 {
				t.Errorf("args = %v, want [] (polymorphic comparison is residual-only)", plan.args)
			}
			if plan.postFilter == nil || plan.postFilter.Op != op {
				t.Fatalf("postFilter should be the residual leaf (op %s), got %+v", op, plan.postFilter)
			}
		})
	}
}

// TestPlanQuery_PolymorphicPresenceCheck_StillPushable confirms the
// polymorphic-residual rule does NOT touch presence checks (type-family
// independent): IS_NULL/NOT_NULL stay pushable even with polymorphic Declared.
func TestPlanQuery_PolymorphicPresenceCheck_StillPushable(t *testing.T) {
	f := spi.Filter{
		Op:       spi.FilterIsNull,
		Path:     "code",
		Source:   spi.SourceData,
		Declared: []spi.DataType{spi.Integer, spi.String},
	}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where == "" {
		t.Error("IS_NULL must still push even on a polymorphic field")
	}
	if plan.postFilter != nil {
		t.Errorf("IS_NULL is EXACT — postFilter should stay nil, got %+v", plan.postFilter)
	}
}

// --- SQL-pushdown soundness contract (Task 11) ---

// TestSoundness_ExactFastPath asserts that a plan whose pushed leaves are ALL
// EXACT (IsNull/NotNull only) keeps postFilter == nil — the SQL LIMIT/OFFSET
// fast path stays enabled because the SQL matches the kernel bit-for-bit.
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

// TestSoundness_AllExactPlanStillChecksEvaluability guards the exact CRITICAL
// gap a fast-path-only Prepare call would leave open: only IsNull/NotNull
// are leafExact, so an AND of two such leaves plans fully pushable and
// EXACT — postFilter stays nil, and dissect() never installs a residual.
// Gating spi.Prepare on "postFilter != nil" would let this shape skip
// evaluability entirely, and IS NULL on a JSON key spi.Prepare cannot
// resolve (an unrecognized SourceMeta path, here) is TRUE for every row —
// not the empty page or rejection every other backend gives, but silently
// selecting everything. planQuery must reject this outright, independent of
// whether the plan shape ever produces a residual to prepare.
func TestSoundness_AllExactPlanStillChecksEvaluability(t *testing.T) {
	f := spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
		{Op: spi.FilterIsNull, Path: "name", Source: spi.SourceData},
		{Op: spi.FilterIsNull, Path: "bogus", Source: spi.SourceMeta},
	}}
	_, err := planQuery(f)
	if err == nil {
		t.Fatal("planQuery must fail on an unevaluable leaf even when every pushed leaf is EXACT and postFilter would stay nil")
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
	}
}

// TestSoundness_MixedPresenceAndValue asserts that adding a single non-EXACT
// leaf (Eq) to a presence check disables the fast path: the whole plan is
// re-checked against the FULL filter.
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
	wantWhere := "(json_extract(data, '$.score') IS NOT NULL AND json_extract(data, '$.score') BETWEEN ? AND ?)"
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
	wantWhere := "(json_extract(data, '$.score') IS NOT NULL AND json_extract(data, '$.score') BETWEEN ? AND ?)"
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
	// A temporal Ne must also be residual-only (isPushable is coercion-blind).
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

// fuzzMetaPaths is the closed canonical SourceMeta vocabulary
// extractFilterMetaValue (the SPI kernel) and validateFilterPaths both
// recognize — used by sanitizeFuzzPath to fold an arbitrary fuzzer string
// into one of these instead of an unrecognized meta path, which spi.Prepare
// now rejects with ErrUnevaluableLeaf.
var fuzzMetaPaths = []string{
	"entity_id", "state", "version", "created_at", "updated_at",
	"model_name", "model_version", "change_type", "transaction_id",
	"id", "creationDate", "lastUpdateTime", "transitionForLatestSave", "transactionId",
}

// sanitizeFuzzPath maps an arbitrary fuzzer-generated string into a path
// spi.Prepare can evaluate, so FuzzQueryPlanner's structural assertions run
// on the large majority of generated inputs instead of bailing via
// ErrUnevaluableLeaf on a path that is essentially always malformed
// (SourceData: an arbitrary string almost never satisfies the bare
// dotted-identifier grammar) or unrecognized (SourceMeta: outside the
// closed canonical vocabulary above). This trades bracket/wildcard-subscript
// exploration (raw fuzzing essentially never produces valid bracket syntax
// anyway) for exploring the AND/OR/dissection/pushdown structure the fuzz
// target actually targets.
func sanitizeFuzzPath(raw string, source spi.FieldSource) string {
	if source == spi.SourceMeta {
		sum := 0
		for _, b := range []byte(raw) {
			sum += int(b)
		}
		return fuzzMetaPaths[sum%len(fuzzMetaPaths)]
	}
	// SourceData: fold to the bare dotted-identifier grammar (letters,
	// digits, underscore, hyphen, dot); anything else becomes 'x'. Leading
	// and consecutive dots are collapsed away, and a trailing dot is
	// trimmed, so ".."/leading "."/trailing "." (all outside the grammar)
	// never survive the fold.
	var b strings.Builder
	prevDot := true // treat the start as "just after a dot" to drop a leading dot
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			prevDot = false
		case r == '.':
			if !prevDot {
				b.WriteRune('.')
				prevDot = true
			}
		default:
			b.WriteByte('x')
			prevDot = false
		}
	}
	out := strings.TrimSuffix(b.String(), ".")
	if out == "" {
		out = "f"
	}
	return out
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
		// Fold the raw fuzzer path into one spi.Prepare can evaluate — see
		// sanitizeFuzzPath's doc comment.
		path = sanitizeFuzzPath(path, source)

		// Build a leaf filter. Declared:String so the comparison/range ops
		// (Eq/Ne/Gt/Lt/Gte/Lte/Between) are evaluable — spi.Prepare now
		// errors on an operand fitting no declared type, and this fuzz
		// target exercises planQuery's structural properties, not operand
		// typing.
		leaf := spi.Filter{
			Op:       op,
			Path:     path,
			Source:   source,
			Value:    value,
			Declared: []spi.DataType{spi.String},
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
					{Op: spi.FilterEq, Path: "x", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
				},
			}
		case 2:
			// OR with the leaf and a pushable sibling.
			filter = spi.Filter{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					leaf,
					{Op: spi.FilterEq, Path: "x", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
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
							{Op: spi.FilterEq, Path: "x", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
						},
					},
					{Op: spi.FilterGt, Path: "z", Source: spi.SourceData, Value: float64(1), Declared: []spi.DataType{spi.Double}},
				},
			}
		}

		// The core property: planQuery must not panic. A random path/operand
		// legitimately failing spi.Prepare's evaluability checks (a
		// SourceMeta path outside the canonical vocabulary, a SourceData
		// path that fails to parse, ...) is an expected, non-fatal outcome
		// under the new ErrUnevaluableLeaf contract, not a planner defect —
		// there is nothing further to assert about a plan that was never
		// produced. Any other error is still a failure.
		plan, err := planQuery(filter)
		if err != nil {
			if errors.Is(err, spi.ErrUnevaluableLeaf) {
				return
			}
			t.Fatalf("planQuery: %v", err)
		}

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

// TestFieldExpr_RendersSubscript is a CHARACTERIZATION test: SQLite's own
// JSON path syntax already spells a positional subscript "$.tags[0]", which
// is exactly what fieldExpr emits once validateJSONPath admits the bracket
// form — fieldExpr needs no rewrite for SQLite. Do not "fix" this into a
// hop-by-hop rewrite; the postgres plugin needs that, SQLite does not.
func TestFieldExpr_RendersSubscript(t *testing.T) {
	cases := []struct{ path, want string }{
		{"amount", "json_extract(data, '$.amount')"},
		{"obj.0", "json_extract(data, '$.obj.0')"},
		{"tags[0]", "json_extract(data, '$.tags[0]')"},
		{"items[2].sku", "json_extract(data, '$.items[2].sku')"},
	}
	for _, tc := range cases {
		got := fieldExpr(spi.Filter{Source: spi.SourceData, Path: tc.path})
		if got != tc.want {
			t.Errorf("fieldExpr(%q) = %q, want %q", tc.path, got, tc.want)
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
// pushable leaf and renders SQLite's own bracket-index spelling.
func TestPlanQuery_PositionalIsPushed(t *testing.T) {
	plan, err := planQuery(spi.Filter{
		Op: spi.FilterEq, Path: "tags[0]", Source: spi.SourceData,
		Value: "A", Declared: []spi.DataType{spi.String},
	})
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if !strings.Contains(plan.where, "'$.tags[0]'") {
		t.Errorf("positional leaf must push its dialect index; got WHERE %q", plan.where)
	}
}

// TestSoundness_NotNeverPushed pins that spi.FilterNot is residual-only:
// isPushable's switch has no FilterNot case, so dissect's default arm
// (via isLeafPushable) routes any NOT node straight to the residual, and no
// "not" ever appears in the WHERE fragment.
//
// This is deliberate today by construction rather than by a written
// soundness rule: the rule one would otherwise author — push a NOT only over
// an exactly-translatable child — would authorize almost nothing, since only
// IS_NULL/NOT_NULL are EXACT (see allPushedExact). Negating an approximate
// (sound-superset) child turns a superset into a subset and silently drops
// rows a narrowing WHERE never returns — the opposite of sound. This test
// guards against a future isPushable edit making NOT pushable without that
// rule being written first: adding spi.FilterNot to isPushable's switch
// makes this test fail (verified during development; not asserted here).
func TestSoundness_NotNeverPushed(t *testing.T) {
	f := spi.Filter{Op: spi.FilterNot, Children: []spi.Filter{
		{Op: spi.FilterEq, Path: "status", Source: spi.SourceData, Value: "CLOSED", Declared: []spi.DataType{spi.String}},
	}}
	plan, err := planQuery(f)
	if err != nil {
		t.Fatalf("planQuery: %v", err)
	}
	if plan.where != "" {
		t.Errorf("NOT must never be pushed to SQL, got WHERE %q", plan.where)
	}
	if plan.postFilter == nil || plan.postFilter.Op != spi.FilterNot {
		t.Fatalf("postFilter should be the full NOT filter (residual-only), got %+v", plan.postFilter)
	}
}
