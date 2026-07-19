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

func matchLifecycle(c *predicate.LifecycleCondition, meta spi.EntityMeta) (bool, error) {
	var fieldValue string

	switch c.Field {
	case "state":
		fieldValue = meta.State
	case "creationDate":
		fieldValue = meta.CreationDate.Format(time.RFC3339Nano)
	case "previousTransition", "transitionForLatestSave":
		fieldValue = meta.TransitionForLatestSave
	default:
		return false, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}

	// Wrap the field value in a gjson.Result for uniform operator dispatch.
	fakeJSON := fmt.Sprintf(`{"v":%q}`, fieldValue)
	result := gjson.Get(fakeJSON, "v")

	return applyOperator(c.OperatorType, result, c.Value)
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
