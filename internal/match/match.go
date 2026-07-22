package match

import (
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// Match evaluates a predicate.Condition against entity data and metadata, returning
// true if the entity satisfies the condition.
func Match(condition predicate.Condition, entityData []byte, entityMeta spi.EntityMeta) (bool, error) {
	switch c := condition.(type) {
	case *predicate.SimpleCondition:
		return matchSimple(c, entityData)
	case *predicate.LifecycleCondition:
		return matchLifecycle(c, entityMeta)
	case *predicate.GroupCondition:
		return matchGroup(c, entityData, entityMeta)
	case *predicate.ArrayCondition:
		return matchArray(c, entityData)
	case *predicate.FunctionCondition:
		return false, fmt.Errorf("function conditions not implemented")
	default:
		return false, fmt.Errorf("unknown condition type: %T", condition)
	}
}

// convertJSONPath converts JSONPath notation to gjson path format.
// Examples:
//
//	"$.name"                    → "name"
//	"$.laureates[*].motivation" → "laureates.#.motivation"
//	"$.address.street"          → "address.street"
//	"name"                      → "name" (already gjson format)
func convertJSONPath(jsonPath string) string {
	path := jsonPath

	// Strip leading "$." or "$"
	if strings.HasPrefix(path, "$.") {
		path = path[2:]
	} else if strings.HasPrefix(path, "$") {
		path = path[1:]
	}

	// Convert array wildcard [*] to gjson # notation.
	path = strings.ReplaceAll(path, "[*]", ".#")

	// Clean up any double dots from the conversion.
	for strings.Contains(path, "..") {
		path = strings.ReplaceAll(path, "..", ".")
	}

	return path
}

func matchSimple(c *predicate.SimpleCondition, data []byte) (bool, error) {
	path := convertJSONPath(c.JsonPath)
	result := gjson.GetBytes(data, path)

	// If the path produced an array result (from # wildcard), check if ANY
	// element matches for applicable operators.
	if result.IsArray() {
		return matchArrayWildcard(c.OperatorType, result, c.Value)
	}

	return applyOperator(c.OperatorType, result, c.Value)
}

// matchArrayWildcard checks if any element in an array result matches the operator.
func matchArrayWildcard(operatorType string, arrayResult gjson.Result, expected any) (bool, error) {
	var lastErr error
	matched := false

	arrayResult.ForEach(func(_, value gjson.Result) bool {
		ok, err := applyOperator(operatorType, value, expected)
		if err != nil {
			lastErr = err
			return false // stop iteration
		}
		if ok {
			matched = true
			return false // short-circuit
		}
		return true // continue
	})

	if lastErr != nil {
		return false, lastErr
	}
	return matched, nil
}

// matchLifecycle evaluates a lifecycle (meta) condition. Field routing is
// identity-driven, never operand-driven: creationDate/lastUpdateTime always
// compare chronologically (epoch-ms) regardless of operator, and the
// remaining canonical meta fields compare lexically via the existing
// string-operator mechanism. See spec §6.3-§6.5 (temporal-search-filters
// design) for the canonical vocabulary and the rationale for unconditional
// field-identity routing.
func matchLifecycle(c *predicate.LifecycleCondition, meta spi.EntityMeta) (bool, error) {
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}

	switch field {
	case "creationDate":
		return matchTemporalMeta(c.OperatorType, meta.CreationDate, c.Value)
	case "lastUpdateTime":
		return matchTemporalMeta(c.OperatorType, meta.LastModifiedDate, c.Value)
	case "state":
		return applyStringLifecycle(c, meta.State)
	case "transitionForLatestSave":
		return applyStringLifecycle(c, meta.TransitionForLatestSave)
	case "transactionId":
		return applyStringLifecycle(c, meta.TransactionID)
	case "id":
		return applyStringLifecycle(c, meta.ID)
	default:
		return false, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}
}

// applyStringLifecycle preserves the pre-existing lexical lifecycle-field
// evaluation: wrap the field value in a fake gjson document and dispatch
// through the same applyOperator used by simple/array conditions.
func applyStringLifecycle(c *predicate.LifecycleCondition, value string) (bool, error) {
	fakeJSON := fmt.Sprintf(`{"v":%q}`, value)
	result := gjson.Get(fakeJSON, "v")
	return applyOperator(c.OperatorType, result, c.Value)
}

// matchTemporalMeta compares a stored meta time.Time chronologically
// (epoch-ms) against the condition operand(s), via the shared
// spi.CompareTemporal dispatcher. Field routing already established that
// this is a temporal field (matchLifecycle); this function does not sniff
// the operand.
func matchTemporalMeta(op string, stored time.Time, value any) (bool, error) {
	storedMs, storedOK := stored.UnixMilli(), !stored.IsZero()

	if op == "BETWEEN" || op == "BETWEEN_INCLUSIVE" {
		lo, hi, ok := twoTemporalBounds(value)
		return spi.CompareTemporal(spi.FilterBetween, storedMs, storedOK, lo, hi, ok), nil
	}

	ms, ok := spi.ParseTemporalMillis(fmt.Sprint(value))
	return spi.CompareTemporal(mapOpToFilterOp(op), storedMs, storedOK, ms, 0, ok), nil
}

// mapOpToFilterOp maps the predicate comparison operator names to spi.FilterOp
// for the temporal dispatcher. BETWEEN is handled separately by the caller.
// An operator with no temporal meaning (e.g. CONTAINS reaching this function
// unvalidated) maps to the zero value, which spi.CompareTemporal treats as
// "no match" — matchLifecycle degrades safely rather than erroring; the
// search boundary is what rejects invalid operator/temporal-field
// combinations (spec §6.4).
func mapOpToFilterOp(op string) spi.FilterOp {
	switch op {
	case "EQUALS":
		return spi.FilterEq
	case "NOT_EQUAL":
		return spi.FilterNe
	case "GREATER_THAN":
		return spi.FilterGt
	case "LESS_THAN":
		return spi.FilterLt
	case "GREATER_OR_EQUAL":
		return spi.FilterGte
	case "LESS_OR_EQUAL":
		return spi.FilterLte
	default:
		return ""
	}
}

// twoTemporalBounds parses a BETWEEN operand ([]any of exactly two values)
// into (lo, hi) epoch-ms. ok is false if the shape is wrong or either bound
// fails to parse — matchTemporalMeta then excludes/vacuous-matches per the
// shared CompareTemporal rule rather than erroring.
func twoTemporalBounds(value any) (lo, hi int64, ok bool) {
	values, isSlice := value.([]any)
	if !isSlice || len(values) != 2 {
		return 0, 0, false
	}
	lo, lok := spi.ParseTemporalMillis(fmt.Sprint(values[0]))
	hi, hok := spi.ParseTemporalMillis(fmt.Sprint(values[1]))
	return lo, hi, lok && hok
}

func matchGroup(c *predicate.GroupCondition, data []byte, meta spi.EntityMeta) (bool, error) {
	switch c.Operator {
	case "AND":
		for _, child := range c.Conditions {
			ok, err := Match(child, data, meta)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil // short-circuit
			}
		}
		return true, nil

	case "OR":
		for _, child := range c.Conditions {
			ok, err := Match(child, data, meta)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil // short-circuit
			}
		}
		return false, nil

	default:
		return false, fmt.Errorf("unknown group operator: %s", c.Operator)
	}
}

func matchArray(c *predicate.ArrayCondition, data []byte) (bool, error) {
	basePath := convertJSONPath(c.JsonPath)

	for i, expected := range c.Values {
		if expected == nil {
			continue // skip null positions
		}

		elemPath := fmt.Sprintf("%s.%d", basePath, i)
		result := gjson.GetBytes(data, elemPath)

		// Delegate to opEquals: it handles numeric-aware comparison
		// (gjson.Number actual + numeric expected) consistently with
		// scalar EQUALS, so per-element semantics don't diverge from
		// scalar EQUALS.
		if !result.Exists() || !opEquals(result, expected) {
			return false, nil
		}
	}

	return true, nil
}

// --- spi.Filter-based evaluation (used by Iterable/GroupedAggregator/streaming-tally) ---
//
// MatchFilter delegates to spi.MatchFilter, the canonical evaluator shared
// with plugins/sqlite's post-filter step. Keeping a single implementation
// (rather than a duplicate copy in this package) is what prevents drift
// between the in-process evaluator (memory Iterate, residual post-filter,
// streaming tally) and the sqlite backend's post-filter — see
// e2e/parity/MatchFilterSqliteEvaluateFilterParity (the smoke test that pins
// this contract) and TestMatchFilter_SqliteParity_Smoke.

// MatchFilter evaluates an spi.Filter against an entity. Filter is the
// pushdown-friendly subset of predicate.Condition used by GroupedAggregator,
// Iterable, and the existing Searcher. Used by the memory plugin's Iterate
// to apply filters inside Next() and by the streaming-tally path when a
// pushdown leaves a residual.
func MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
	return spi.MatchFilter(f, data, meta)
}
