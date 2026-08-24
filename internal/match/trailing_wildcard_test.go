package match

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// A path whose LAST hop is an array wildcard ("$.tags[*]") addresses the array's
// ELEMENTS. The rewrite mapped every "[*]" to gjson's "#", which is correct
// mid-path (gjson projects "#" across elements) but wrong at the end: a trailing
// "#" yields the array's LENGTH. So `$.tags[*] EQUALS "red"` compared "red"
// against 2 and never matched — an empty page on search, and a workflow
// criterion that silently never fires. A wrong-but-available answer, which the
// project forbids.
//
// The same arithmetic broke every path that ends in an array VALUE rather than
// a scalar, however it gets there:
//
//   - "$.matrix[*][*]" — two trailing wildcards; "matrix.#.#" yielded the inner
//     arrays' lengths ([2,2]).
//   - "$.a[*].b[*]" — a wildcard behind a projection; "a.#.b.#" likewise.
//   - "$.orders[*].lines[*].sku" — NO trailing wildcard at all, but two
//     projections, so gjson nests one array per hop ([[ "S1","S2" ],["S3"]])
//     and the per-element comparison compared a scalar against an ARRAY.
//
// All three are the same defect: the gjson result must be a FLAT array of the
// values the path addresses, because prepLeaf iterates it exactly once.

// TestConvertJSONPath_TrailingWildcard pins the rewrite itself.
func TestConvertJSONPath_TrailingWildcard(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// A trailing "[*]" addresses the ARRAY; prepLeaf's array branch
		// iterates it with existential (ANY) semantics.
		{"trailing wildcard", "$.tags[*]", "tags"},
		{"trailing wildcard after dots", "$.a.b[*]", "a.b"},
		{"trailing wildcard after index", "$.a[0].b[*]", "a.0.b"},

		// Mid-path "[*]" keeps projecting.
		{"mid-path wildcard", "$.laureates[*].motivation", "laureates.#.motivation"},
		{"wildcard then index", "$.a[*].b[0]", "a.#.b.0"},
		{"wildcard then index segment", "$.a[*][0]", "a.#.0"},

		// One "@flatten" per array level beyond the first, so the result is a
		// flat array of the addressed values.
		{"nested trailing wildcards", "$.matrix[*][*]", "matrix|@flatten"},
		{"projection then trailing wildcard", "$.a[*].b[*]", "a.#.b|@flatten"},
		{"projection then dotted trailing wildcard", "$.a[*].b.c[*]", "a.#.b.c|@flatten"},
		{"two projections, scalar leaf", "$.orders[*].lines[*].sku", "orders.#.lines.#.sku|@flatten"},
		{"two projections and a trailing wildcard", "$.a[*].b[*].c[*]", "a.#.b.#.c|@flatten|@flatten"},

		// Degenerate spellings the boundary grammar rejects but internally
		// constructed paths may still reach: the rewrite stays total.
		{"dotted bracket", "$.a.[*]", "a"},
		{"wildcard only", "$.[*]", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := convertJSONPath(tc.in); got != tc.want {
				t.Errorf("convertJSONPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

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

		// An EMPTY array has no element to satisfy either presence test —
		// existential semantics, vacuously false. Under the count rewrite
		// NOT_NULL was TRUE here, because the length 0 is a present number.
		{"empty array NOT_NULL", "$.empty[*]", "NOT_NULL", nil, nil, false},
		{"empty array IS_NULL", "$.empty[*]", "IS_NULL", nil, nil, false},
		{"non-empty array NOT_NULL", "$.tags[*]", "NOT_NULL", nil, nil, true},
		{"missing array NOT_NULL", "$.nope[*]", "NOT_NULL", nil, nil, false},
		{"missing array IS_NULL", "$.nope[*]", "IS_NULL", nil, nil, true},
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
// container path is spelled with a trailing wildcard still addresses the array's
// positions. arrayElementFieldPath already treats "$.tags" and "$.tags[*]" as
// naming the same element type; prepArray appends ".N" to the converted base, so
// the two spellings must converge on the same gjson base too.
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
