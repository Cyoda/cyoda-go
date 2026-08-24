package search_test

import (
	"errors"
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
			if err := search.ValidateCondition(&predicate.ArrayCondition{
				JsonPath: tc.path, Values: []any{"v"},
			}); err != nil {
				t.Fatalf("ValidateCondition(array jsonPath=%q) = %v, want accepted", tc.path, err)
			}
		})
	}
}

// TestValidateCondition_PathGrammarMatchesSPI is the anti-drift guard. The
// boundary check is a port of spi.ConditionToFilter's own path grammar, and
// the two must classify identically: whatever the translator refuses with
// spi.ErrInvalidFilterPath the boundary must reject 400, and whatever it
// refuses as merely non-pushdownable the boundary must accept so the
// in-memory fallback still serves it.
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
