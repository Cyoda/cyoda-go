package match

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// The two evaluators must answer identically. They are reached by different
// routes for the same request — a backend's residual re-check runs the SPI
// kernel, an untranslatable condition and every workflow criterion run this
// one — so a disagreement is a request answering differently depending on its
// query plan.
func TestEvaluatorsAgree(t *testing.T) {
	str := []spi.DataType{spi.String}
	fieldTypes := func(string) []spi.DataType { return str }

	docs := []string{
		`{"a":"A"}`, `{"a":["A","B"]}`, `{"a":[]}`, `{"a":null}`, `{}`,
		`{"obj":{"0":"Z"}}`, `{"items":[{"sku":"A"},{}]}`,
	}
	paths := []string{"$.a", "$.a[*]", "$.a[0]", "$.obj.0", "$.items[*].sku"}
	ops := []string{"EQUALS", "NOT_EQUAL", "CONTAINS", "IS_NULL", "NOT_NULL"}

	for _, doc := range docs {
		for _, p := range paths {
			for _, op := range ops {
				cond := &predicate.SimpleCondition{JsonPath: p, OperatorType: op, Value: "A"}

				prep, err := Prepare(cond, fieldTypes)
				if err != nil {
					t.Fatalf("match.Prepare(%s %s): %v", p, op, err)
				}
				gotMatch := prep.Match([]byte(doc), spi.EntityMeta{})

				f, err := spi.ConditionToFilter(cond, map[string]spi.FieldDescriptor{
					"$.a[*]": {Types: str}, "$.a": {Types: str},
					"$.obj.0": {Types: str}, "$.items[*].sku": {Types: str},
				})
				if err != nil {
					t.Fatalf("spi.ConditionToFilter(%s %s): %v", p, op, err)
				}
				gotKernel := spi.Prepare(f).Match([]byte(doc), spi.EntityMeta{})

				if gotMatch != gotKernel {
					t.Errorf("doc=%s path=%s op=%s: match=%v kernel=%v",
						doc, p, op, gotMatch, gotKernel)
				}
			}
		}
	}
}

// TestMatch_TemporalMetaField_StringOperatorNeverMatches pins the REAL
// contract for a string or pattern operator applied to a temporal meta field
// (creationDate, lastUpdateTime): this evaluator never-matches it,
// unconditionally, for every caller — it does NOT align with the SPI
// kernel's lexical RFC3339-text match, and the two evaluators' disagreement
// here is deliberate and permanent, not a gap to close.
//
// Why this evaluator cannot align with the kernel: a workflow criterion is
// validated once at import and then stored verbatim; every subsequent save
// calls this package's Prepare directly with NO revalidation
// (workflow/engine.go). Every OTHER match.Prepare call site is preceded by
// the shared validation boundary (search.validateLifecycleType), which now
// rejects this predicate on every validated surface — but that one call site
// is not, so a criterion imported before the boundary existed still reaches
// this evaluator with the predicate the system has since declared
// unsupported. Never-match is the fail-closed answer required here
// (.claude/rules/correctness-over-availability.md); a lexical match would
// silently reactivate that criterion's dormant transition on a binary
// upgrade alone, with no import and no operator action.
//
// There is also no "two doors" problem to reconcile for a criterion the way
// there is for a search condition: a criterion never routes through the SPI
// kernel at all, so path-grammar.md section 10's "one resolver" requirement
// — which governs cases reachable through EITHER of two query plans for the
// SAME request — does not describe this caller.
//
// hasKnownTemporalMetaDivergence (prepared_equivalence_test.go) excludes
// exactly this case from the property-test corpus for the same reason: the
// corpus deliberately bypasses the validation boundary, and the two
// evaluators are not required to agree on an unvalidated predicate.
func TestMatch_TemporalMetaField_StringOperatorNeverMatches(t *testing.T) {
	meta := spi.EntityMeta{
		CreationDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		LastModifiedDate: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	fields := []string{"creationDate", "lastUpdateTime"}
	cases := []struct {
		op    string
		value string
	}{
		{"CONTAINS", "2024"},
		{"NOT_CONTAINS", "2024"},
		{"STARTS_WITH", "2024"},
		{"NOT_STARTS_WITH", "2024"},
		{"ENDS_WITH", "Z"},
		{"NOT_ENDS_WITH", "Z"},
		{"LIKE", "2024%"},
		{"MATCHES_PATTERN", "^2024.*"},
		{"IEQUALS", "2024-01-01t00:00:00z"},
		{"ICONTAINS", "2024"},
	}

	for _, field := range fields {
		for _, tc := range cases {
			// Each operand is deliberately chosen so it WOULD lexically
			// match the field's RFC3339 rendering (creationDate is
			// 2024-01-01T00:00:00Z, lastUpdateTime is 2024-06-01T00:00:00Z)
			// — a false "match=false" from a value that could never have
			// matched anyway would not distinguish never-match from a real
			// evaluation that happens to disagree.
			cond := &predicate.LifecycleCondition{Field: field, OperatorType: tc.op, Value: tc.value}

			prep, err := Prepare(cond, nil)
			if err != nil {
				t.Fatalf("match.Prepare(%s %s): %v", field, tc.op, err)
			}
			if got := prep.Match(nil, meta); got {
				t.Errorf("field=%s op=%s: match=%v, want false (never-match, unconditionally)", field, tc.op, got)
			}
		}
	}
}
