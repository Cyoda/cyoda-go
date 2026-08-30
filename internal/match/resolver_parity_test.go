package match

import (
	"testing"

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
