package match

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// A numeric array subscript ("$.arr[0]") is valid JSON Path, is ACCEPTED by
// search.ValidateConditionJSONPath, and is refused by spi.ConditionToFilter
// with a plain (non-ErrInvalidFilterPath) error — which every call site reads
// as "not pushdownable, evaluate in memory". So the in-memory evaluator is the
// ONLY evaluator that ever sees it, and it is obliged to resolve it.
//
// It did not. convertJSONPath rewrote "[*]" to gjson's "#" but left "[0]"
// alone, and gjson has no bracket syntax — it addresses an array element as
// "arr.0". "arr[0]" therefore reached gjson as a request for a key literally
// spelled "arr[0]", missed, and the leaf evaluated false for every entity:
// an empty page (or a criterion that never fires) for a field that holds the
// value. That is a wrong-but-available answer, which the project forbids.

// TestConvertJSONPath_Subscripts is the unit table for the rewrite itself.
func TestConvertJSONPath_Subscripts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Plain dotted paths — unchanged.
		{"simple", "$.name", "name"},
		{"dotted", "$.address.street", "address.street"},
		{"no leader", "name", "name"},
		{"dollar only leader", "$name", "name"},
		{"already gjson index", "$.arr.0", "arr.0"},

		// Wildcards — the pre-existing behaviour, pinned so the numeric
		// rewrite cannot regress it.
		{"wildcard leaf", "$.tags[*]", "tags.#"},
		{"wildcard mid", "$.laureates[*].motivation", "laureates.#.motivation"},
		{"wildcard twice", "$.a[*].b[*].c", "a.#.b.#.c"},

		// Numeric subscripts — the fix.
		{"index leaf", "$.arr[0]", "arr.0"},
		{"index multi-digit", "$.arr[10]", "arr.10"},
		{"index mid", "$.arr[0].name", "arr.0.name"},
		{"index nested", "$.a[0].b[1]", "a.0.b.1"},
		{"index chained", "$.a[0][1]", "a.0.1"},
		{"index then wildcard", "$.a[0].b[*]", "a.0.b.#"},
		{"wildcard then index", "$.a[*].b[0]", "a.#.b.0"},
		{"index at root segment", "$.0", "0"},

		// Shapes with no gjson equivalent are left VERBATIM rather than
		// half-rewritten — see the doc comment on convertJSONPath.
		{"negative index", "$.arr[-1]", "arr[-1]"},
		{"slice", "$.arr[0:2]", "arr[0:2]"},
		{"union", "$.arr[0,1]", "arr[0,1]"},
		{"filter expression", "$.arr[?(@.x)]", "arr[?(@.x)]"},
		{"unclosed", "$.arr[0", "arr[0"},
		{"empty subscript", "$.arr[]", "arr[]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := convertJSONPath(tc.in); got != tc.want {
				t.Errorf("convertJSONPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// numericSubscriptDoc holds a value at every shape the table below addresses.
const numericSubscriptDoc = `{
	"arr": [10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110],
	"items": [{"name": "first"}, {"name": "second"}],
	"a": [{"b": ["x", "y"]}, {"b": ["p", "q"]}]
}`

// TestPrepare_NumericSubscriptResolves is the behavioural proof: a condition on
// a numeric-subscript path must match the entity that holds the value there.
// Before the fix every one of these evaluated false.
func TestPrepare_NumericSubscriptResolves(t *testing.T) {
	tests := []struct {
		name     string
		jsonPath string
		value    any
		declared []spi.DataType
		want     bool
	}{
		{"leaf index", "$.arr[0]", 10, []spi.DataType{spi.Integer}, true},
		{"leaf index no match", "$.arr[0]", 999, []spi.DataType{spi.Integer}, false},
		{"multi-digit index", "$.arr[10]", 110, []spi.DataType{spi.Integer}, true},
		{"index mid-path", "$.items[1].name", "second", []spi.DataType{spi.String}, true},
		{"index nested", "$.a[1].b[0]", "p", []spi.DataType{spi.String}, true},
		{"wildcard then index", "$.a[*].b[1]", "q", []spi.DataType{spi.String}, true},
		// A subscript BEHIND a wildcard hop still resolves per element: gjson's
		// mid-path "#" maps over the array, and prepLeaf's IsArray branch
		// short-circuits on the first element that matches.
		{"wildcard then index other element", "$.a[*].b[1]", "y", []spi.DataType{spi.String}, true},
	}
	// Deliberately absent: a path ENDING in "[*]" ("$.a[0].b[*]"). It converts
	// to "a.0.b.#", and a trailing gjson "#" yields the array's COUNT, not its
	// elements — so an equality on it compares against a number. That is
	// pre-existing trailing-wildcard behaviour, unrelated to numeric
	// subscripts, and load-bearing for TestSearchSort_PushdownFallbackAgree
	// (which relies on "$.tags[*] NOT_NULL" being true via the count). The
	// conversion itself is pinned in TestConvertJSONPath_Subscripts.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cond := &predicate.SimpleCondition{
				JsonPath: tc.jsonPath, OperatorType: "EQUALS", Value: tc.value,
			}
			p, err := Prepare(cond, func(string) []spi.DataType { return tc.declared })
			if err != nil {
				t.Fatalf("Prepare(%q): %v", tc.jsonPath, err)
			}
			if got := p.Match([]byte(numericSubscriptDoc), spi.EntityMeta{}); got != tc.want {
				t.Errorf("Match on %q = %v, want %v", tc.jsonPath, got, tc.want)
			}
		})
	}
}

// TestPrepareArray_NumericSubscriptBase pins that an ArrayCondition whose
// container path itself carries a subscript addresses the right sub-array.
// prepArray appends ".N" to arrayBase, so a half-rewritten base produced
// "a[0].b.0" — a miss on every position, i.e. a condition false for every row.
func TestPrepareArray_NumericSubscriptBase(t *testing.T) {
	cond := &predicate.ArrayCondition{JsonPath: "$.a[1].b", Values: []any{"p", "q"}}
	p, err := Prepare(cond, func(string) []spi.DataType { return []spi.DataType{spi.String} })
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !p.Match([]byte(numericSubscriptDoc), spi.EntityMeta{}) {
		t.Error(`ArrayCondition on "$.a[1].b" with values ["p","q"] did not match — the subscripted base did not resolve`)
	}
}
