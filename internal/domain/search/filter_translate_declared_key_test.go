package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// FieldsMap keys are always "$."-prefixed (normalisePath). A condition may
// legitimately omit the prefix — TestConditionToFilter_SimpleNoPrefix pins that
// "city" is accepted — and validateConditionPaths normalises before checking, so
// such a path passes every gate as a known field.
//
// The translator must normalise too. Looking up the raw path misses the map,
// leaves Declared nil, and a type-directed comparison leaf with no declared type
// expands into nothing and never matches. The caller gets 200 with an empty page
// for a field that exists and holds matching data.
func fieldsFixture() map[string]schema.FieldDescriptor {
	return map[string]schema.FieldDescriptor{
		"$.city":   {Path: "$.city", Types: []spi.DataType{spi.String}},
		"$.amount": {Path: "$.amount", Types: []spi.DataType{spi.Integer}},
		"$.when":   {Path: "$.when", Types: []spi.DataType{spi.ZonedDateTime}},
	}
}

func TestConditionToFilter_DeclaredResolvesForPrefixlessPath(t *testing.T) {
	fields := fieldsFixture()

	for _, path := range []string{"city", "$.city"} {
		t.Run(path, func(t *testing.T) {
			f, err := ConditionToFilter(&predicate.SimpleCondition{
				JsonPath:     path,
				OperatorType: "EQUALS",
				Value:        "Berlin",
			}, fields)
			if err != nil {
				t.Fatalf("ConditionToFilter: %v", err)
			}
			if len(f.Declared) != 1 || f.Declared[0] != spi.String {
				t.Fatalf("Declared = %v, want [STRING] — an unresolved declared set makes the leaf never match", f.Declared)
			}
			if !spi.MatchFilter(f, []byte(`{"city":"Berlin"}`), spi.EntityMeta{}) {
				t.Error("leaf did not match an entity that holds the value")
			}
		})
	}
}

// Coercion is looked up with the same key and has the same defect: a
// declared-temporal field reached by a prefix-less path was stamped CoerceNone,
// so the SQL planners would compare it as text rather than as a timestamp.
func TestConditionToFilter_CoercionResolvesForPrefixlessPath(t *testing.T) {
	fields := fieldsFixture()

	for _, path := range []string{"when", "$.when"} {
		t.Run(path, func(t *testing.T) {
			f, err := ConditionToFilter(&predicate.SimpleCondition{
				JsonPath:     path,
				OperatorType: "GREATER_THAN",
				Value:        "2020-01-01T00:00:00Z",
			}, fields)
			if err != nil {
				t.Fatalf("ConditionToFilter: %v", err)
			}
			if f.Coercion != spi.CoerceTemporal {
				t.Errorf("Coercion = %v, want CoerceTemporal", f.Coercion)
			}
		})
	}
}

// A genuinely unknown path must still resolve to no declared types — the
// deliberate degrade-to-non-match behaviour this must not disturb.
func TestConditionToFilter_UnknownPathStillHasNoDeclaredTypes(t *testing.T) {
	f, err := ConditionToFilter(&predicate.SimpleCondition{
		JsonPath:     "$.nosuch",
		OperatorType: "EQUALS",
		Value:        "x",
	}, fieldsFixture())
	if err != nil {
		t.Fatalf("ConditionToFilter: %v", err)
	}
	if len(f.Declared) != 0 {
		t.Errorf("Declared = %v, want empty for an unknown path", f.Declared)
	}
	if f.Coercion != spi.CoerceNone {
		t.Errorf("Coercion = %v, want CoerceNone for an unknown path", f.Coercion)
	}
}
