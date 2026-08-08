package match

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// Every caller resolves declared types out of a FieldsMap whose keys carry the
// "$." prefix — the workflow engine (engine.go), the search fallback loop
// (search/service.go) and the grouped-stats residual all index that same map.
//
// prepareSimple (née matchSimple) looked the path up raw, so a criterion or
// condition written without the prefix resolved to no declared type. A
// type-directed comparison with no declared type expands into nothing and
// never matches, so the leaf evaluated false for every entity. prepareArray
// already normalised via arrayElementFieldPath; prepareSimple did not.
//
// This is the same defect fixed in filter_translate.go, on the path workflow
// criteria actually take: criteria always go through match.Prepare /
// (Prepared).Match and never through ConditionToFilter.
func prefixedTypes(t *testing.T) FieldTypes {
	t.Helper()
	fields := map[string][]spi.DataType{
		"$.amount": {spi.Integer},
		"$.city":   {spi.String},
	}
	return func(p string) []spi.DataType { return fields[p] }
}

func TestMatchSimple_ResolvesDeclaredTypesForPrefixlessPath(t *testing.T) {
	data := []byte(`{"amount":42,"city":"Berlin"}`)

	cases := []struct {
		name     string
		jsonPath string
		op       string
		value    any
	}{
		{"prefixed numeric", "$.amount", "GREATER_THAN", 10},
		{"prefixless numeric", "amount", "GREATER_THAN", 10},
		{"prefixed string", "$.city", "EQUALS", "Berlin"},
		{"prefixless string", "city", "EQUALS", "Berlin"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cond := &predicate.SimpleCondition{
				JsonPath:     tc.jsonPath,
				OperatorType: tc.op,
				Value:        tc.value,
			}
			prepared, err := Prepare(cond, prefixedTypes(t))
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if !prepared.Match(data, spi.EntityMeta{}) {
				t.Errorf("%s did not match an entity that holds the value — declared types did not resolve", tc.jsonPath)
			}
		})
	}
}

// A genuinely unknown path must still resolve to no declared types, so a
// comparison leaf keeps degrading to non-match rather than erroring.
func TestMatchSimple_UnknownPathStillDegradesToNonMatch(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.nosuch",
		OperatorType: "GREATER_THAN",
		Value:        10,
	}
	prepared, err := Prepare(cond, prefixedTypes(t))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Match([]byte(`{"amount":42}`), spi.EntityMeta{}) {
		t.Error("an unknown path matched")
	}
}
