package postgres

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPlanQuery_BoolEqNe guards against the pgx "cannot find encode plan"
// failure: a boolean operand must be bound as its text form ("true"/"false")
// so it encodes against the text-typed doc->>'path' extraction, matching the
// lexicographic text comparison used for strings and the memory/sqlite backends.
func TestPlanQuery_BoolEqNe(t *testing.T) {
	tests := []struct {
		name    string
		op      spi.FilterOp
		value   bool
		wantSQL string
		wantArg string
	}{
		{"eq_true", spi.FilterEq, true, "(doc->>'active' IS NOT NULL AND doc->>'active' = $1)", "true"},
		{"eq_false", spi.FilterEq, false, "(doc->>'active' IS NOT NULL AND doc->>'active' = $1)", "false"},
		{"ne_true", spi.FilterNe, true, "(doc->>'active' IS NULL OR doc->>'active' != $1)", "true"},
		{"ne_false", spi.FilterNe, false, "(doc->>'active' IS NULL OR doc->>'active' != $1)", "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := spi.Filter{
				Op:     tt.op,
				Path:   "active",
				Source: spi.SourceData,
				Value:  tt.value,
			}
			plan := planQuery(f)
			if plan.where != tt.wantSQL {
				t.Errorf("where:\n  got  %s\n  want %s", plan.where, tt.wantSQL)
			}
			if len(plan.args) != 1 {
				t.Fatalf("args = %v, want 1 element", plan.args)
			}
			// The arg must be the text form of the bool, never a raw Go bool
			// (pgx cannot encode bool into text OID 25).
			if _, isBool := plan.args[0].(bool); isBool {
				t.Errorf("arg is a raw bool %v; must be bound as text to avoid pgx encode failure", plan.args[0])
			}
			if plan.args[0] != tt.wantArg {
				t.Errorf("args[0] = %#v, want %q", plan.args[0], tt.wantArg)
			}
		})
	}
}
