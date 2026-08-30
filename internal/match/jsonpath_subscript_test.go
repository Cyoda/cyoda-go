package match

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// A numeric array subscript ("$.arr[0]") is valid JSON Path, is ACCEPTED by
// search.ValidateConditionJSONPath, and is a well-formed statement the SPI
// kernel resolves directly via spi.ResolvePath. This evaluator shares that
// same resolver (spi.ParseFilterPath + spi.ResolvePath), so it is obliged to
// resolve it identically — a positional subscript is no longer this
// evaluator's private concern, it is a property of the one shared grammar.
//
// The rewrite this file used to pin (convertJSONPath, deleted) hand-translated
// JSONPath into gjson's own bracket-less notation and mishandled numeric
// subscripts: it rewrote "[*]" to gjson's "#" but left "[0]" alone, and gjson
// has no bracket syntax — it addresses an array element as "arr.0". "arr[0]"
// therefore reached gjson as a request for a key literally spelled "arr[0]",
// missed, and the leaf evaluated false for every entity: an empty page (or a
// criterion that never fires) for a field that holds the value. That is a
// wrong-but-available answer, which the project forbids. The behavioural
// proof below (TestPrepare_NumericSubscriptResolves) is what remains: the
// unit table for the deleted rewrite itself is gone, because the rewrite it
// pinned no longer exists — that behaviour now lives in the SPI
// (spi.ParseFilterPath / spi.ResolvePath), which has its own tests.

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
		// A subscript BEHIND a wildcard hop still resolves per element:
		// spi.ResolvePath expands the wildcard hop into one result per
		// element and then applies the following "[1]" hop to each, and
		// prepLeaf's existential loop matches on the first result that
		// satisfies the leaf.
		{"wildcard then index other element", "$.a[*].b[1]", "y", []spi.DataType{spi.String}, true},
		// A path ENDING in "[*]" behind a positional hop: "$.a[0].b[*]"
		// addresses the ELEMENTS of that one sub-array. It used to convert to
		// "a.0.b.#" — a trailing gjson "#" is the array's COUNT — so an equality
		// on it compared against a number and never matched. The trailing-
		// wildcard family is covered in full by
		// TestPrepare_TrailingWildcardIteratesElements; this row is here because
		// it is the numeric-subscript combination.
		{"index then trailing wildcard", "$.a[0].b[*]", "y", []spi.DataType{spi.String}, true},
	}
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
// prepare() runs spi.DesugarCondition on the condition before dispatching, so
// this ArrayCondition becomes an AND of positional EQUALS leaves — each one
// addressing an element of the subscripted base path (e.g. "$.a[1].b[0]") —
// and is resolved the same way any other leaf is, through spi.ResolvePath.
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
