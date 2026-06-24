package postgres_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestCyodaTryFloat8 covers the D21 truth table from the grouped-stats design spec §6.3.
// The helper must:
//   - return NULL for empty, non-numeric, NaN, ±Infinity, and inputs with
//     trailing whitespace (the \A/\Z anchors reject any leading/trailing
//     character including newlines that ^/$ would tolerate);
//   - return NULL for finite-grammar inputs that overflow to ±Infinity
//     during ::float8 coercion (e.g. "1e500");
//   - return the parsed value for every well-formed finite numeric.
func TestCyodaTryFloat8(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() {
		_ = postgres.DropSchemaForTest(pool)
	})

	cases := []struct {
		in       string
		wantNull bool
		wantVal  float64
	}{
		{"", true, 0},
		{"abc", true, 0},
		{"NaN", true, 0},
		{"Infinity", true, 0},
		{"-Infinity", true, 0},
		{"42", false, 42},
		{"-1.5", false, -1.5},
		{"1e10", false, 1e10},
		{"1e500", true, 0},      // overflow → 22003 → EXCEPTION → NULL
		{"-1e500", true, 0},     // negative overflow → 22003 → NULL
		{"1e400", true, 0},      // float8 max ~1.8e308; 1e400 also overflows
		{"42\n", true, 0},       // trailing newline rejected by \Z anchor
		{"\n42", true, 0},       // leading newline rejected by \A anchor
		{" 42", true, 0},        // leading whitespace rejected
		{"42 ", true, 0},        // trailing whitespace rejected
		{"42.5e10", false, 42.5e10},
		{"0", false, 0},
		{"-0", false, 0},
		{"3.14", false, 3.14},
		{"1e-3", false, 1e-3},
		{"+42", true, 0}, // grammar requires no leading +
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var got *float64
			err := pool.QueryRow(context.Background(),
				"SELECT cyoda_try_float8($1)", tc.in).Scan(&got)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if tc.wantNull {
				if got != nil {
					t.Fatalf("got %v, want NULL", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got NULL, want %v", tc.wantVal)
			}
			// Use a tight tolerance to allow IEEE round-trip noise on the
			// scientific-notation cases without masking real regressions.
			if math.Abs(*got-tc.wantVal) > math.Abs(tc.wantVal)*1e-12 {
				t.Fatalf("got %v, want %v", *got, tc.wantVal)
			}
		})
	}
}

// TestEntitiesStateIdx confirms the partial expression index from D19
// exists after migration and references the canonical
// doc->'_meta'->>'state' expression with the NOT deleted predicate.
func TestEntitiesStateIdx(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() {
		_ = postgres.DropSchemaForTest(pool)
	})

	var indexDef string
	err := pool.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'entities_state_idx'`,
	).Scan(&indexDef)
	if err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	// We assert on substrings rather than the exact serialized form because
	// pg_indexes normalizes the expression (e.g. quoting, casts) per server
	// version. The substrings prove the canonical expression and partial
	// predicate are present.
	wantSubstrs := []string{
		"tenant_id",
		"model_name",
		"model_version",
		"_meta",
		"state",
		"NOT deleted",
	}
	for _, s := range wantSubstrs {
		if !strings.Contains(indexDef, s) {
			t.Errorf("indexdef missing %q: %s", s, indexDef)
		}
	}
}
