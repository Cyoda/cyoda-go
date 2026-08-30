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

// TestEvaluatorsAgree_TemporalMetaField_StringOperator pins that a string or
// pattern operator applied to a temporal meta field (creationDate,
// lastUpdateTime) is evaluated IDENTICALLY by both resolvers.
//
// Before this test, prepareLifecycle guarded a non-temporal operator on a
// temporal field to a never-match on field identity, while the SPI kernel
// bridges the field to its RFC3339 text and applies the operator lexically —
// the divergence both evaluators' own "KNOWN DIVERGENCE" comments named. The
// two evaluators already share the expansion (spi.ExpandLeaf, via leafNode)
// and the evaluation (spi.EvalLeaf, called from Match's prepMetaTemporal
// arm) for every OTHER operator on this field; the guard was the one place
// this evaluator stopped delegating and substituted its own answer instead.
// Removing it completes the delegation match.Prepare already performs for
// prepMetaString and prepLeaf.
func TestEvaluatorsAgree_TemporalMetaField_StringOperator(t *testing.T) {
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
			cond := &predicate.LifecycleCondition{Field: field, OperatorType: tc.op, Value: tc.value}

			prep, err := Prepare(cond, nil)
			if err != nil {
				t.Fatalf("match.Prepare(%s %s): %v", field, tc.op, err)
			}
			gotMatch := prep.Match(nil, meta)

			f, err := spi.ConditionToFilter(cond, nil)
			if err != nil {
				t.Fatalf("spi.ConditionToFilter(%s %s): %v", field, tc.op, err)
			}
			gotKernel := spi.Prepare(f).Match(nil, meta)

			if gotMatch != gotKernel {
				t.Errorf("field=%s op=%s: match=%v kernel=%v", field, tc.op, gotMatch, gotKernel)
			}
		}
	}
}
