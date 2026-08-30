package search_test

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// invalidJSONPaths are the spellings that are not JSON Path nomenclature at
// all. Each must be REJECTED by search.ValidateCondition with an error
// wrapping search.ErrInvalidFieldPath, so every transport answers 400
// INVALID_FIELD_PATH instead of silently falling back to the in-memory
// evaluator (which resolves a bare path happily, hiding the mistake) or
// answering an empty page for a field that exists (bracket-quoted access,
// which no evaluator in the stack resolves).
var invalidJSONPaths = []struct {
	name string
	path string
}{
	{"bare identifier", "amount"},
	{"bare dotted", "address.city"},
	{"bare meta-looking", "_meta.state"},
	{"empty", ""},
	{"bare dollar", "$"},
	{"leader only", "$."},
	{"leading dot", ".amount"},
	{"trailing dot", "$.amount."},
	{"empty segment", "$..amount"},
	{"double dot mid-path", "$.a..b"},
	{"bracket quoted", "$['x']"},
	{"bracket quoted after leader", "$.['x']"},
	{"bracket quoted nested", "$.a.['x']"},
	{"space", "$.foo bar"},
	{"single quote", "$.foo'"},
	{"sql tail", "$.x'; --"},
	{"non-ascii", "$.café"},
	{"asterisk outside subscript", "$.foo*"},
	{"slash", "$.foo/bar"},
	{"embedded dollar", "$.foo$bar"},
	{"filter expression", "$.foo?(@.x)"},
	{"colon", "$.foo:bar"},
	{"at sign", "$.@"},
	{"nul byte", "$.foo\x00"},
	// The in-memory evaluator's own metacharacters, excluded deliberately. Two
	// of them do not merely fail to name a field, they name a DIFFERENT one:
	// gjson reads "|" as an alternative segment separator (so "$.foo|bar"
	// resolves nested foo→bar, the "." collision respelled) and "*"/"?" as key
	// wildcards. "!" introduces a literal, "#" is the array count/projection
	// segment, and a backslash is the escape. None can appear in a field name —
	// schema.IsSegmentNameByte refuses them at the model door — so admitting
	// one here could only reach gjson as an instruction.
	{"gjson key wildcard question", "$.foo?bar"},
	{"gjson count-or-projection segment", "$.#"},
	{"gjson count-or-projection in segment", "$.foo#"},
	{"gjson segment separator pipe", "$.foo|bar"},
	{"gjson literal bang", "$.!true"},
	{"gjson escape backslash", `$.foo\bar`},

	// Malformed BRACKET spellings. These are the ones the grammar used to let
	// through: it stopped scanning at the first '[' and accepted whatever
	// followed, so each classified as "valid but unpushdownable" — which every
	// engine caller reads as "fall back to in-memory evaluation", where gjson
	// resolves none of them. The result was an empty page (or a criterion that
	// never fires) for a field that exists, and on the two surfaces with no
	// downstream schema backstop — grouped-stats conditions and workflow
	// criterion import — a 200 with wrong buckets.
	{"unclosed subscript", "$.a["},
	{"unclosed subscript with index", "$.a[0"},
	{"unmatched close", "$.a]"},
	{"unmatched close mid-path", "$.a].b"},
	{"subscript without field", "$.[0]"},
	{"wildcard without field", "$.[*]"},
	{"empty subscript", "$.a[]"},
	{"negative index", "$.a[-1]"},
	{"signed index", "$.a[+1]"},
	{"exponent index", "$.a[1e2]"},
	{"slice", "$.a[0:2]"},
	{"union", "$.a[0,1]"},
	{"filter expression subscript", "$.a[?(@.x)]"},
	{"filter expression in later segment", "$.a[*].b[?(x)]"},
	{"double-quoted subscript", `$.a["x"]`},
	{"single-quoted subscript", "$.a['x']"},
	{"bracket quoted chained onto index", "$.a[0]['x']"},
	{"whitespace in subscript", "$.a[ 0]"},
	{"whitespace after index", "$.a[0 ]"},
	{"sql tail after subscript", "$.a[0];DROP"},
	{"sql comment after subscript", "$.a[0]'; --"},
	{"non-ascii after subscript", "$.a[0].xé"},
	{"name glued to subscript", "$.a[0]b"},
	{"slash after subscript", "$.a[0]/etc"},
	{"nul after subscript", "$.a[0]\x00"},
	{"empty segment after subscript", "$.a[*]..b"},
	{"trailing dot after subscript", "$.a[*]."},
	{"non-index chained subscript", "$.tags[*][x]"},
	{"negative chained index", "$.a[0][-1]"},

	// Overflowing digit runs. A digit-class-only check admits any run of
	// digits regardless of magnitude; spi.ParseFilterPath, which the boundary
	// now delegates to directly, additionally requires the run to fit an int
	// (strconv.Atoi). No entity array is ever long enough for these indices
	// to be meaningful, so rejecting is the correct, fail-closed answer — see
	// path-grammar.md §9 and §10.
	{"overflowing index", "$.tags[99999999999999999999]"},
	{"overflowing index mid-path", "$.items[99999999999999999999].name"},
	{"overflowing chained index", "$.matrix[0][99999999999999999999]"},
}

// validJSONPaths must keep working. Two classes are folded together
// deliberately: well-formed dotted paths (pushable), and array-subscripted
// paths which are valid JSON Path but not expressible as a pushdown filter —
// those must NOT be rejected here, because the engine answers them from the
// in-memory evaluator via the translate-error fallback. Rejecting them would
// turn working queries into 400s.
var validJSONPaths = []struct {
	name string
	path string
}{
	{"single segment", "$.x"},
	{"dotted", "$.a.b"},
	{"numeric segment", "$.tags.0"},
	{"underscore leader segment", "$._meta.state"},
	{"hyphen", "$.foo-bar"},
	{"underscore", "$.foo_bar"},
	{"uppercase", "$.FooBar"},
	{"array wildcard", "$.tags[*].name"},
	{"array index", "$.arr[0]"},
	{"array wildcard leaf", "$.tags[*]"},
	{"multi-digit index", "$.arr[12].a.b"},
	{"chained wildcards", "$.matrix[*][*]"},
	{"chained indices", "$.matrix[0][1]"},
	{"mixed chained subscripts", "$.a[0][*].b"},
	{"nested array hops", "$.orders[*].lines[*].sku"},
}

// TestValidateCondition_RejectsNonJSONPath is the reject table. jsonPath is
// JSON Path nomenclature; an invalid path is invalid, not a tolerated alias.
func TestValidateCondition_RejectsNonJSONPath(t *testing.T) {
	for _, tc := range invalidJSONPaths {
		t.Run("simple/"+tc.name, func(t *testing.T) {
			err := search.ValidateCondition(&predicate.SimpleCondition{
				JsonPath: tc.path, OperatorType: "EQUALS", Value: "v",
			})
			if err == nil {
				t.Fatalf("ValidateCondition(jsonPath=%q) = nil, want rejection", tc.path)
			}
			if !errors.Is(err, search.ErrInvalidFieldPath) {
				t.Fatalf("ValidateCondition(jsonPath=%q) error %v does not wrap ErrInvalidFieldPath", tc.path, err)
			}
		})
		t.Run("array/"+tc.name, func(t *testing.T) {
			err := search.ValidateCondition(&predicate.ArrayCondition{
				JsonPath: tc.path, Values: []any{"v"},
			})
			if err == nil {
				t.Fatalf("ValidateCondition(array jsonPath=%q) = nil, want rejection", tc.path)
			}
			if !errors.Is(err, search.ErrInvalidFieldPath) {
				t.Fatalf("ValidateCondition(array jsonPath=%q) error %v does not wrap ErrInvalidFieldPath", tc.path, err)
			}
		})
		t.Run("nested-in-group/"+tc.name, func(t *testing.T) {
			// A malformed path buried under a group must still be found: the
			// translator short-circuits on the FIRST child that fails, so a
			// preceding non-pushdownable sibling would otherwise mask it.
			err := search.ValidateCondition(&predicate.GroupCondition{
				Operator: "AND",
				Conditions: []predicate.Condition{
					&predicate.SimpleCondition{JsonPath: "$.tags[*].name", OperatorType: "EQUALS", Value: "x"},
					&predicate.SimpleCondition{JsonPath: tc.path, OperatorType: "EQUALS", Value: "v"},
				},
			})
			if !errors.Is(err, search.ErrInvalidFieldPath) {
				t.Fatalf("ValidateCondition(group containing %q) error %v does not wrap ErrInvalidFieldPath", tc.path, err)
			}
		})
	}
}

// TestValidateCondition_AcceptsValidJSONPath is the positive control. A
// tightening that breaks valid callers is worse than the bug it fixes.
func TestValidateCondition_AcceptsValidJSONPath(t *testing.T) {
	for _, tc := range validJSONPaths {
		t.Run("simple/"+tc.name, func(t *testing.T) {
			if err := search.ValidateCondition(&predicate.SimpleCondition{
				JsonPath: tc.path, OperatorType: "EQUALS", Value: "v",
			}); err != nil {
				t.Fatalf("ValidateCondition(jsonPath=%q) = %v, want accepted", tc.path, err)
			}
		})
		t.Run("array/"+tc.name, func(t *testing.T) {
			// validJSONPaths pins GRAMMAR validity, which every entry here
			// has. The `array` clause layers one more requirement on top of
			// the grammar (path-grammar.md §8): the jsonPath must carry a
			// trailing "[*]", since the clause tests elements by position and
			// a path that does not address elements cannot. So a grammar-valid
			// path here is accepted for the array clause only when it also
			// ends in "[*]" — everything else in this table is still
			// GRAMMATICALLY fine (that's what makes it a member of
			// validJSONPaths) but rejected for THIS clause specifically, the
			// same rejection TestValidateCondition_ArrayClauseRequiresWildcard
			// pins directly.
			err := search.ValidateCondition(&predicate.ArrayCondition{
				JsonPath: tc.path, Values: []any{"v"},
			})
			if strings.HasSuffix(tc.path, "[*]") {
				if err != nil {
					t.Fatalf("ValidateCondition(array jsonPath=%q) = %v, want accepted", tc.path, err)
				}
				return
			}
			if !errors.Is(err, search.ErrInvalidFieldPath) {
				t.Fatalf("ValidateCondition(array jsonPath=%q) = %v, want rejected (no trailing \"[*]\")", tc.path, err)
			}
		})
	}
}

// TestValidateCondition_PathGrammarMatchesSPI is the anti-drift guard. The
// boundary check is built directly on spi.ParseFilterPath (the same
// function spi.ConditionToFilter's translator consults), so the two classify
// identically by construction — this pins that against the translator
// itself: whatever it refuses with spi.ErrInvalidFilterPath the boundary
// must reject 400, and every well-formed path (subscripts included, since
// the kernel now resolves them directly — see spi.ResolvePath) the boundary
// must accept.
func TestValidateCondition_PathGrammarMatchesSPI(t *testing.T) {
	all := append(append([]struct{ name, path string }{}, invalidJSONPaths...), validJSONPaths...)
	for _, tc := range all {
		t.Run(tc.name, func(t *testing.T) {
			cond := &predicate.SimpleCondition{JsonPath: tc.path, OperatorType: "EQUALS", Value: "v"}
			_, translateErr := spi.ConditionToFilter(cond, nil)
			spiRejects := errors.Is(translateErr, spi.ErrInvalidFilterPath)

			boundaryRejects := errors.Is(search.ValidateCondition(cond), search.ErrInvalidFieldPath)
			if spiRejects != boundaryRejects {
				t.Fatalf("path %q: spi.ErrInvalidFilterPath=%v but boundary rejection=%v — the two classifications have drifted",
					tc.path, spiRejects, boundaryRejects)
			}
		})
	}
}

// TestValidateCondition_LifecycleFieldIsNotAPath pins that the meta
// vocabulary is untouched by the jsonPath grammar: a LifecycleCondition names
// a member of the closed meta set directly, so a bare "state" stays valid.
func TestValidateCondition_LifecycleFieldIsNotAPath(t *testing.T) {
	for _, field := range []string{"state", "creationDate", "lastUpdateTime"} {
		if err := search.ValidateCondition(&predicate.LifecycleCondition{
			Field: field, OperatorType: "EQUALS", Value: "x",
		}); err != nil {
			t.Errorf("ValidateCondition(lifecycle field %q) = %v, want accepted", field, err)
		}
	}
}
