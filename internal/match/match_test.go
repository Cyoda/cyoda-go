package match

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

func meta() spi.EntityMeta {
	return spi.EntityMeta{
		ID:                      "entity-999",
		State:                   "CREATED",
		CreationDate:            time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		TransitionForLatestSave: "workflow.step1",
		TransactionID:           "tx-123",
	}
}

var sampleData = []byte(`{
	"name": "Alice",
	"age": 30,
	"score": 85.5,
	"active": true,
	"city": null,
	"tags": ["go", "rust", "python"],
	"address": {"street": "Main St", "zip": "12345"},
	"laureates": [
		{"name": "Bob", "motivation": "for peace"},
		{"name": "Carol", "motivation": "for chemistry"}
	]
}`)

// --- 1. Simple EQUALS ---

func TestMatchSimpleEqualsString(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleEqualsStringFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Bob"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

func TestMatchSimpleEqualsNumber(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "EQUALS", Value: float64(30)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleEqualsNumberAsString(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "EQUALS", Value: "30"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for numeric string comparison")
	}
}

func TestMatchSimpleEqualsBool(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.active", OperatorType: "EQUALS", Value: "true"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

// --- 2. Simple NOT_EQUAL ---

func TestMatchSimpleNotEqual(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "NOT_EQUAL", Value: "Bob"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleNotEqualFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "NOT_EQUAL", Value: "Alice"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// --- 3. IS_NULL / NOT_NULL ---

func TestMatchSimpleIsNull(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.city", OperatorType: "IS_NULL"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for null field")
	}
}

func TestMatchSimpleIsNullMissing(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.nonexistent", OperatorType: "IS_NULL"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for missing field")
	}
}

func TestMatchSimpleNotNull(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "NOT_NULL"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleNotNullOnNull(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.city", OperatorType: "NOT_NULL"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false for null field")
	}
}

// --- 4. GREATER_THAN / LESS_THAN ---

func TestMatchSimpleGreaterThan(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "GREATER_THAN", Value: float64(25)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleGreaterThanFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "GREATER_THAN", Value: float64(30)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

func TestMatchSimpleLessThan(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "LESS_THAN", Value: float64(35)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleLessThanFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "LESS_THAN", Value: float64(30)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// --- 5. GREATER_OR_EQUAL / LESS_OR_EQUAL ---

func TestMatchSimpleGreaterOrEqual(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "GREATER_OR_EQUAL", Value: float64(30)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for equal value")
	}
}

func TestMatchSimpleLessOrEqual(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "LESS_OR_EQUAL", Value: float64(30)}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for equal value")
	}
}

// --- 6. CONTAINS / NOT_CONTAINS ---

func TestMatchSimpleContains(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "CONTAINS", Value: "lic"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleNotContains(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "NOT_CONTAINS", Value: "xyz"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

// --- 7. STARTS_WITH / ENDS_WITH ---

func TestMatchSimpleStartsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "STARTS_WITH", Value: "Ali"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleNotStartsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "NOT_STARTS_WITH", Value: "Bob"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleEndsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "ENDS_WITH", Value: "ice"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleNotEndsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "NOT_ENDS_WITH", Value: "xyz"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

// --- 8. IEQUALS / ICONTAINS ---

func TestMatchSimpleIEquals(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "IEQUALS", Value: "alice"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for case-insensitive equals")
	}
}

func TestMatchSimpleINotEqual(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "INOT_EQUAL", Value: "alice"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false for case-insensitive not-equal on match")
	}
}

func TestMatchSimpleIContains(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "ICONTAINS", Value: "LIC"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for case-insensitive contains")
	}
}

func TestMatchSimpleINotContains(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "INOT_CONTAINS", Value: "XYZ"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleIStartsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "ISTARTS_WITH", Value: "ALI"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleINotStartsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "INOT_STARTS_WITH", Value: "BOB"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleIEndsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "IENDS_WITH", Value: "ICE"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleINotEndsWith(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "INOT_ENDS_WITH", Value: "XYZ"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

// --- 9. MATCHES_PATTERN ---

func TestMatchSimpleMatchesPattern(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "MATCHES_PATTERN", Value: "^A.*e$"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleMatchesPatternFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "MATCHES_PATTERN", Value: "^B.*"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// --- 10. LIKE ---

func TestMatchSimpleLike(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "LIKE", Value: "A%"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for LIKE A%")
	}
}

func TestMatchSimpleLikeUnderscore(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "LIKE", Value: "Alic_"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for LIKE Alic_")
	}
}

func TestMatchSimpleLikeFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "LIKE", Value: "B%"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// --- 11. BETWEEN ---

func TestMatchSimpleBetweenString(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "BETWEEN", Value: "25,35"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for 30 between 25 and 35")
	}
}

func TestMatchSimpleBetweenSlice(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "BETWEEN", Value: []any{float64(25), float64(35)}}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchSimpleBetweenInclusiveEdge(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "BETWEEN_INCLUSIVE", Value: "30,30"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for edge-inclusive")
	}
}

// --- 11. Lifecycle condition ---

func TestMatchLifecycleStateMatch(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "CREATED"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchLifecycleStateNoMatch(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "DELETED"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// TestMatchLifecycleCreationDate_ContainsIsNotTemporal proves creationDate is
// no longer lexically matched: CONTAINS is not a valid comparison operator for
// a temporal field (spec §6.4 — rejected at validation on validated entry
// points), and matchLifecycle degrades safely to no-match rather than falling
// back to substring matching on the formatted date string (the pre-fix
// behaviour this test used to pin).
func TestMatchLifecycleCreationDate_ContainsIsNotTemporal(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2026-01-15"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false: CONTAINS is not a valid temporal comparison operator")
	}
}

func TestMatchLifecycleCreationDate_GreaterThan(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_THAN", Value: "2026-01-15T00:00:00Z"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true: 2026-01-15T10:30:00Z > 2026-01-15T00:00:00Z")
	}
}

func TestMatchLifecycleCreationDate_LessThan(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "LESS_THAN", Value: "2026-01-15T00:00:00Z"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false: 2026-01-15T10:30:00Z is not < 2026-01-15T00:00:00Z")
	}
}

func TestMatchLifecycleCreationDate_NotEqual(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "NOT_EQUAL", Value: "2020-01-01T00:00:00Z"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true: creationDate does not equal an unrelated instant")
	}
}

func TestMatchLifecycleCreationDate_Between(t *testing.T) {
	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2026-01-01T00:00:00Z", "2026-01-31T00:00:00Z"},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true: creationDate falls within the January 2026 range")
	}
}

func TestMatchLifecycleCreationDate_BetweenOutsideRange(t *testing.T) {
	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2020-01-01T00:00:00Z", "2020-01-31T00:00:00Z"},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false: creationDate falls outside the 2020 range")
	}
}

// TestMatchLifecycleCreationDate_UnsetIsExcluded pins spec §7.1: a stored
// value that does not convert to ms (zero time.Time) is EXCLUDED for
// comparison ops, and vacuously true for NOT_EQUAL.
func TestMatchLifecycleCreationDate_UnsetIsExcluded(t *testing.T) {
	unsetMeta := spi.EntityMeta{State: "CREATED"} // CreationDate is zero-value
	cond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "EQUALS", Value: "2026-01-15T10:30:00Z"}
	got, err := matchLifecycle(cond, unsetMeta)
	if err != nil || got {
		t.Errorf("expected exclude (false) for unset stored value; got=%v err=%v", got, err)
	}

	neCond := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "NOT_EQUAL", Value: "2026-01-15T10:30:00Z"}
	got, err = matchLifecycle(neCond, unsetMeta)
	if err != nil || !got {
		t.Errorf("expected vacuous true for NOT_EQUAL on unset stored value; got=%v err=%v", got, err)
	}
}

// TestMatchLifecycle_TemporalEquals is the driving RED test from the task
// brief: creationDate EQUALS must compare chronologically (epoch-ms), not
// lexically, so an operand with a differently-formatted-but-identical
// instant still matches.
func TestMatchLifecycle_TemporalEquals(t *testing.T) {
	m := spi.EntityMeta{CreationDate: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "EQUALS", Value: "2021-01-01T00:00:00.000Z"}
	ok, err := matchLifecycle(c, m)
	if err != nil || !ok {
		t.Errorf("EQUALS same-instant should match; ok=%v err=%v", ok, err)
	}
}

func TestMatchLifecycle_LastUpdateTime(t *testing.T) {
	m := spi.EntityMeta{LastModifiedDate: time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC)}
	c := &predicate.LifecycleCondition{Field: "lastUpdateTime", OperatorType: "GREATER_THAN", Value: "2021-06-01T11:00:00Z"}
	ok, err := matchLifecycle(c, m)
	if err != nil || !ok {
		t.Errorf("lastUpdateTime GT earlier should match; ok=%v err=%v", ok, err)
	}
}

// TestMatchLifecycle_TemporalGteLte pins the boundary-inclusive behavior of
// the GREATER_OR_EQUAL / LESS_OR_EQUAL temporal branches in mapOpToFilterOp:
// GE/LE of the exact stored instant matches, and GE of a strictly later
// instant does not.
func TestMatchLifecycle_TemporalGteLte(t *testing.T) {
	instant := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	m := spi.EntityMeta{CreationDate: instant}

	geExact := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_OR_EQUAL", Value: "2026-01-15T10:30:00Z"}
	if ok, err := matchLifecycle(geExact, m); err != nil || !ok {
		t.Errorf("GE of the exact instant should match; ok=%v err=%v", ok, err)
	}

	leExact := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "LESS_OR_EQUAL", Value: "2026-01-15T10:30:00Z"}
	if ok, err := matchLifecycle(leExact, m); err != nil || !ok {
		t.Errorf("LE of the exact instant should match; ok=%v err=%v", ok, err)
	}

	geLater := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_OR_EQUAL", Value: "2026-01-15T10:30:00.001Z"}
	if ok, err := matchLifecycle(geLater, m); err != nil || ok {
		t.Errorf("GE of a later instant should not match; ok=%v err=%v", ok, err)
	}
}

// TestMatchLifecycle_TemporalBetweenUnsetExcluded pins spec §7.1 for BETWEEN:
// a zero-value stored CreationDate (storedOK=false) is excluded, never a
// vacuous match, for BETWEEN.
func TestMatchLifecycle_TemporalBetweenUnsetExcluded(t *testing.T) {
	unsetMeta := spi.EntityMeta{} // CreationDate is zero-value
	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2026-01-01T00:00:00Z", "2026-01-31T00:00:00Z"},
	}
	got, err := matchLifecycle(cond, unsetMeta)
	if err != nil || got {
		t.Errorf("expected exclude (false) for BETWEEN against unset stored value; got=%v err=%v", got, err)
	}
}

// TestMatchLifecycle_TemporalBetweenMalformedOperand exercises
// twoTemporalBounds' ok=false branches: a malformed bound set must degrade
// safely to no-match rather than panicking.
func TestMatchLifecycle_TemporalBetweenMalformedOperand(t *testing.T) {
	m := spi.EntityMeta{CreationDate: time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)}

	oneElement := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"2026-01-01T00:00:00Z"}, // wrong shape: only one bound
	}
	if got, err := matchLifecycle(oneElement, m); err != nil || got {
		t.Errorf("expected no-match for malformed (1-element) BETWEEN operand; got=%v err=%v", got, err)
	}

	nonRFC3339Bound := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        []any{"not-a-timestamp", "2026-01-31T00:00:00Z"}, // lo bound not offset-RFC3339
	}
	if got, err := matchLifecycle(nonRFC3339Bound, m); err != nil || got {
		t.Errorf("expected no-match for malformed (non-RFC3339 bound) BETWEEN operand; got=%v err=%v", got, err)
	}
}

func TestMatchLifecycle_TransactionId(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "transactionId", OperatorType: "EQUALS", Value: "tx-123"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for transactionId match")
	}
}

func TestMatchLifecycle_ID(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "id", OperatorType: "EQUALS", Value: "entity-999"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for id match")
	}
}

func TestMatchLifecycle_UnknownFieldError(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "bogusField", OperatorType: "EQUALS", Value: "x"}
	_, err := Match(cond, sampleData, meta())
	if err == nil {
		t.Error("expected error for unknown lifecycle field")
	}
}

func TestMatchLifecycleTransition(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "transitionForLatestSave", OperatorType: "EQUALS", Value: "workflow.step1"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchLifecyclePreviousTransition(t *testing.T) {
	cond := &predicate.LifecycleCondition{Field: "previousTransition", OperatorType: "EQUALS", Value: "workflow.step1"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for previousTransition alias")
	}
}

// --- 12. Group AND ---

func TestMatchGroupAndAllMatch(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
			&predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "EQUALS", Value: float64(30)},
		},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchGroupAndOneFails(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
			&predicate.SimpleCondition{JsonPath: "$.age", OperatorType: "EQUALS", Value: float64(99)},
		},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// --- 13. Group OR ---

func TestMatchGroupOrOneMatches(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "OR",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Bob"},
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
		},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestMatchGroupOrNoneMatch(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "OR",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Bob"},
			&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Carol"},
		},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false")
	}
}

// --- 14. Nested group ---

func TestMatchNestedGroupAndContainingOr(t *testing.T) {
	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.active", OperatorType: "EQUALS", Value: "true"},
			&predicate.GroupCondition{
				Operator: "OR",
				Conditions: []predicate.Condition{
					&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Bob"},
					&predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"},
				},
			},
		},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

// --- 15. Array condition ---

func TestMatchArrayCondition(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.tags",
		Values:   []any{"go", nil, "python"},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true: index 0=go, index 1=skip, index 2=python")
	}
}

func TestMatchArrayConditionMismatch(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.tags",
		Values:   []any{"go", nil, "java"},
	}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false: index 2 is python not java")
	}
}

// --- 16. Function condition → error ---

func TestMatchFunctionConditionError(t *testing.T) {
	cond := &predicate.FunctionCondition{}
	_, err := Match(cond, sampleData, meta())
	if err == nil {
		t.Error("expected error for function condition")
	}
}

// --- 17. IS_CHANGED → error ---

func TestMatchIsChangedError(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "IS_CHANGED"}
	_, err := Match(cond, sampleData, meta())
	if err == nil {
		t.Error("expected error for IS_CHANGED")
	}
}

func TestMatchIsUnchangedError(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "IS_UNCHANGED"}
	_, err := Match(cond, sampleData, meta())
	if err == nil {
		t.Error("expected error for IS_UNCHANGED")
	}
}

// --- 18. No match: field doesn't exist ---

func TestMatchMissingFieldReturnsFalse(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.nonexistent", OperatorType: "EQUALS", Value: "anything"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false for missing field")
	}
}

// --- Array wildcard with CONTAINS ---

func TestMatchArrayWildcardContains(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.laureates[*].motivation", OperatorType: "CONTAINS", Value: "peace"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true: one laureate motivation contains 'peace'")
	}
}

func TestMatchArrayWildcardContainsNoMatch(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.laureates[*].motivation", OperatorType: "CONTAINS", Value: "physics"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected false: no laureate motivation contains 'physics'")
	}
}

// --- Nested field access ---

func TestMatchNestedField(t *testing.T) {
	cond := &predicate.SimpleCondition{JsonPath: "$.address.street", OperatorType: "EQUALS", Value: "Main St"}
	got, err := Match(cond, sampleData, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true")
	}
}

// --- Issue #24: matchArray numeric-aware comparison ---

func TestMatchArrayCondition_NumericInt(t *testing.T) {
	data := []byte(`{"scores":[1,2,3]}`)
	cond := &predicate.ArrayCondition{
		JsonPath: "$.scores",
		Values:   []any{1, 2, 3}, // Go int
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected match for int values against numeric JSON array")
	}
}

func TestMatchArrayCondition_NumericInt64(t *testing.T) {
	data := []byte(`{"scores":[1,2,3]}`)
	cond := &predicate.ArrayCondition{
		JsonPath: "$.scores",
		Values:   []any{int64(1), int64(2), int64(3)},
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected match for int64 values against numeric JSON array")
	}
}

func TestMatchArrayCondition_NumericFloat64(t *testing.T) {
	data := []byte(`{"scores":[1,2,3]}`)
	cond := &predicate.ArrayCondition{
		JsonPath: "$.scores",
		Values:   []any{1.0, 2.0, 3.0},
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected match for float64 values against numeric JSON array")
	}
}

func TestMatchArrayCondition_JSONNumber(t *testing.T) {
	// Predicates built from XML imports (after PR-2) deliver json.Number.
	data := []byte(`{"scores":[1.5]}`)
	cond := &predicate.ArrayCondition{
		JsonPath: "$.scores",
		Values:   []any{json.Number("1.5")},
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected match for json.Number expected against numeric JSON array")
	}
}

func TestMatchArrayCondition_TypeMismatch(t *testing.T) {
	// String entity field, numeric expected — must NOT match.
	data := []byte(`{"tags":["go"]}`)
	cond := &predicate.ArrayCondition{
		JsonPath: "$.tags",
		Values:   []any{42},
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected no match: numeric expected against string JSON array element")
	}
}

// TestMatchArrayCondition_NumericFormatDivergence proves the regression that
// motivated this fix. Pre-fix, matchArray compared fmt.Sprintf("%v", expected)
// to gjson's result.String(). For float64(1e10), Sprintf renders "1e+10"
// while gjson decimal-expands the JSON literal 1e10 to "10000000000". These
// strings differ even though the values are equal. The pre-fix code would
// return no-match; opEquals does numeric comparison (actual.Float() ==
// toFloat64(expected)) and returns match. This case is the executable proof
// that the change has user-visible behavioral effect.
func TestMatchArrayCondition_NumericFormatDivergence(t *testing.T) {
	data := []byte(`{"scores":[1e10]}`) // gjson.String() = "10000000000"
	cond := &predicate.ArrayCondition{
		JsonPath: "$.scores",
		Values:   []any{float64(1e10)}, // Sprintf renders "1e+10"
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected match: float64(1e10) against JSON 1e10 — numeric equality required, string comparison would fail")
	}
}

// TestMatchArrayCondition_NoStringOperandCoercion pins the transitive effect
// of the numeric-alignment change (#423 §6.6 / #431 seed) on matchArray: it
// delegates per-element comparison to opEquals, which no longer coerces a
// string operand against a numeric element — it falls through to lexical
// comparison instead. A numeric array element (100) must NOT match a
// numeric-looking-but-lexically-different string Values entry ("100.0") at
// the same position: numeric coercion would treat them equal (100 == 100.0),
// but lexical comparison of "100" (actual.String()) vs "100.0" does not.
func TestMatchArrayCondition_NoStringOperandCoercion(t *testing.T) {
	data := []byte(`{"scores":[100]}`)
	cond := &predicate.ArrayCondition{
		JsonPath: "$.scores",
		Values:   []any{"100.0"}, // string operand, numeric JSON element
	}
	got, err := Match(cond, data, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected no match: string operand \"100.0\" must not be numerically coerced against numeric element 100 (lexical \"100\" != \"100.0\")")
	}
}
