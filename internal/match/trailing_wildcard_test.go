package match

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// A path whose LAST hop is an array wildcard ("$.tags[*]") addresses the array's
// ELEMENTS, and every array hop counts, wherever it sits in the path
// (docs/cloud-parity/path-grammar.md section 3):
//
//   - "$.matrix[*][*]" — each element of each inner array.
//   - "$.a[*].b[*]" — each element of each "b".
//   - "$.orders[*].lines[*].sku" — each "sku", across both hops.
//
// spi.ResolvePath is the one resolver both evaluators share, and it never
// nests its result once per hop the way a naive gjson path-string rewrite
// does: it keeps one flat slice of the addressed values at every step, so
// there is no "flatten the extra nesting back out" arithmetic to get wrong
// here at all — see that function's doc comment. Before this evaluator
// delegated to it, the deleted convertJSONPath rewrite got this wrong in
// exactly the ways listed above (a trailing "#" is gjson's array LENGTH, not
// its elements, and two "#" projections nest one array per hop): an empty
// page on search, and a workflow criterion that silently never fired. The
// unit table that pinned that rewrite's output is gone along with it — the
// behavioural proof below is what remains.

// trailingWildcardDoc holds a value at every shape the behavioural table below
// addresses.
const trailingWildcardDoc = `{
	"tags": ["red", "blue"],
	"empty": [],
	"matrix": [[1, 2], [3, 4]],
	"a": [{"b": ["x", "y"]}, {"b": ["p", "q"]}],
	"orders": [{"lines": [{"sku": "S1"}, {"sku": "S2"}]}, {"lines": [{"sku": "S3"}]}],
	"mixed": [{"k": 1}, "scalar"],
	"items": [{"price": 10}, {"price": 20}]
}`

// TestPrepare_TrailingWildcardIteratesElements is the behavioural proof: a
// condition on a path that ends in an array must be answered from that array's
// ELEMENTS, never from its length.
func TestPrepare_TrailingWildcardIteratesElements(t *testing.T) {
	str := []spi.DataType{spi.String}
	num := []spi.DataType{spi.Integer}

	tests := []struct {
		name     string
		jsonPath string
		op       string
		value    any
		declared []spi.DataType
		want     bool
	}{
		{"first element", "$.tags[*]", "EQUALS", "red", str, true},
		{"last element", "$.tags[*]", "EQUALS", "blue", str, true},
		{"absent element", "$.tags[*]", "EQUALS", "green", str, false},
		// The killer assertion: 2 is the array's LENGTH. Matching it would
		// prove the leaf is still comparing against the count.
		{"length is not an element", "$.tags[*]", "EQUALS", 2, num, false},
		{"string operator per element", "$.tags[*]", "CONTAINS", "lu", str, true},

		// Nested arrays flatten to the values the path addresses.
		{"nested wildcards", "$.matrix[*][*]", "EQUALS", 3, num, true},
		{"nested wildcards absent", "$.matrix[*][*]", "EQUALS", 9, num, false},
		{"nested wildcards inner length", "$.matrix[*][*]", "EQUALS", 2, num, true}, // 2 IS an element
		{"projection then trailing", "$.a[*].b[*]", "EQUALS", "q", str, true},
		{"projection then trailing absent", "$.a[*].b[*]", "EQUALS", "zz", str, false},
		{"two projections scalar leaf", "$.orders[*].lines[*].sku", "EQUALS", "S3", str, true},
		{"two projections scalar leaf absent", "$.orders[*].lines[*].sku", "EQUALS", "S9", str, false},

		// A polymorphic object-or-scalar element compares against its scalar
		// occurrences; the object occurrences simply do not match.
		{"polymorphic scalar occurrence", "$.mixed[*]", "EQUALS", "scalar", str, true},

		// Unchanged shapes, pinned so the rewrite cannot regress them.
		{"mid-path unchanged", "$.items[*].price", "EQUALS", 20, num, true},
		{"index behind trailing wildcard", "$.a[0].b[*]", "EQUALS", "y", str, true},

		// A "[*]" path never answers the array's own nullness — it addresses
		// ELEMENTS and nothing else (docs/cloud-parity/path-grammar.md
		// section 5). An empty array, an explicit null and an absent field
		// are three states of the array, and all three present no elements
		// to a wildcard path, so both presence tests answer false for all
		// three: on a wildcard path IS_NULL and NOT_NULL are NOT complements.
		// Ask about the array itself with a bare "$.empty" / "$.nope", which
		// separates them.
		{"empty array NOT_NULL", "$.empty[*]", "NOT_NULL", nil, nil, false},
		{"empty array IS_NULL", "$.empty[*]", "IS_NULL", nil, nil, false},
		{"non-empty array NOT_NULL", "$.tags[*]", "NOT_NULL", nil, nil, true},
		{"missing array NOT_NULL", "$.nope[*]", "NOT_NULL", nil, nil, false},
		{"missing array IS_NULL", "$.nope[*]", "IS_NULL", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cond := &predicate.SimpleCondition{
				JsonPath: tc.jsonPath, OperatorType: tc.op, Value: tc.value,
			}
			p, err := Prepare(cond, func(string) []spi.DataType { return tc.declared })
			if err != nil {
				t.Fatalf("Prepare(%q %s %v): %v", tc.jsonPath, tc.op, tc.value, err)
			}
			if got := p.Match([]byte(trailingWildcardDoc), spi.EntityMeta{}); got != tc.want {
				t.Errorf("Match(%q %s %v) = %v, want %v", tc.jsonPath, tc.op, tc.value, got, tc.want)
			}
		})
	}
}

// TestPrepareArray_TrailingWildcardBase pins that an ArrayCondition whose
// container path is spelled with a trailing wildcard still addresses the
// array's positions. spi.DesugarCondition treats "$.tags" and "$.tags[*]" as
// naming the same container (see desugarArrayElementPath: a trailing "[*]"
// is REPLACED by "[i]"; a bare path has "[i]" APPENDED), so the two spellings
// must converge on the same positional leaves too.
func TestPrepareArray_TrailingWildcardBase(t *testing.T) {
	for _, path := range []string{"$.tags", "$.tags[*]"} {
		t.Run(path, func(t *testing.T) {
			cond := &predicate.ArrayCondition{JsonPath: path, Values: []any{"red", "blue"}}
			p, err := Prepare(cond, func(string) []spi.DataType { return []spi.DataType{spi.String} })
			if err != nil {
				t.Fatalf("Prepare(%q): %v", path, err)
			}
			if !p.Match([]byte(trailingWildcardDoc), spi.EntityMeta{}) {
				t.Errorf(`ArrayCondition on %q with values ["red","blue"] did not match`, path)
			}
		})
	}
}
