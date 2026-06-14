package match

import (
	"encoding/json"
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

// MatchFilter evaluates an spi.Filter against an entity. Filter is the
// pushdown-friendly subset of predicate.Condition used by GroupedAggregator,
// Iterable, and the existing Searcher. Used by the memory plugin's Iterate
// to apply filters inside Next() and by the streaming-tally path when a
// pushdown leaves a residual.
//
// A zero-value filter (no Op) matches everything. An explicit empty AND
// (Op = FilterAnd with no children) is the AND identity (true). An explicit
// empty OR is the OR identity (false).
//
// Unlike Match, MatchFilter does not return an error. The pushdown contract
// guarantees ops are well-formed before they reach here; an unsupported op
// (which would only happen on a programmer error or SPI/plugin drift) is
// treated as a non-match.
func MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
	// Zero-value filter matches everything. We only check Op because a
	// genuinely-empty filter has no Op; an explicit Op: FilterAnd with no
	// children must fall through to the group evaluator (which returns true).
	if f.Op == "" && f.Path == "" && f.Source == "" && len(f.Children) == 0 && f.Value == nil && f.Values == nil {
		return true
	}
	return evalFilter(f, data, meta)
}

func evalFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
	switch f.Op {
	case spi.FilterAnd:
		for _, c := range f.Children {
			if !evalFilter(c, data, meta) {
				return false
			}
		}
		return true
	case spi.FilterOr:
		for _, c := range f.Children {
			if evalFilter(c, data, meta) {
				return true
			}
		}
		return false
	}
	return evalLeafFilter(f, data, meta)
}

// evalLeafFilter mirrors the sqlite plugin's evaluateLeaf (post_filter.go)
// but takes raw data + meta instead of *spi.Entity so it can be called from
// inner loops without constructing an Entity wrapper.
func evalLeafFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
	// IsNull / NotNull are checked first because they care about presence,
	// not value extraction succeeding.
	switch f.Op {
	case spi.FilterIsNull:
		_, found := extractFilterValue(f, data, meta)
		return !found
	case spi.FilterNotNull:
		val, found := extractFilterValue(f, data, meta)
		return found && val != nil
	}

	val, found := extractFilterValue(f, data, meta)

	// For "negative" ops (Ne, INe, NotContains, NotStartsWith, NotEndsWith),
	// a missing-or-null field is vacuously true; for everything else, missing
	// short-circuits to false.
	isNegativeOp := f.Op == spi.FilterNe ||
		f.Op == spi.FilterINe ||
		f.Op == spi.FilterINotContains ||
		f.Op == spi.FilterINotStartsWith ||
		f.Op == spi.FilterINotEndsWith
	if !found || val == nil {
		return isNegativeOp
	}

	switch f.Op {
	case spi.FilterEq:
		return compareFilterValues(val, f.Value) == 0
	case spi.FilterNe:
		return compareFilterValues(val, f.Value) != 0
	case spi.FilterGt:
		return compareFilterValues(val, f.Value) > 0
	case spi.FilterLt:
		return compareFilterValues(val, f.Value) < 0
	case spi.FilterGte:
		return compareFilterValues(val, f.Value) >= 0
	case spi.FilterLte:
		return compareFilterValues(val, f.Value) <= 0
	case spi.FilterContains:
		return strings.Contains(fmt.Sprint(val), fmt.Sprint(f.Value))
	case spi.FilterStartsWith:
		return strings.HasPrefix(fmt.Sprint(val), fmt.Sprint(f.Value))
	case spi.FilterEndsWith:
		return strings.HasSuffix(fmt.Sprint(val), fmt.Sprint(f.Value))
	case spi.FilterLike:
		ok, err := opLike(toGjsonResult(val), f.Value)
		return err == nil && ok
	case spi.FilterBetween:
		if len(f.Values) < 2 {
			return false
		}
		return compareFilterValues(val, f.Values[0]) >= 0 &&
			compareFilterValues(val, f.Values[1]) <= 0
	case spi.FilterMatchesRegex:
		ok, err := opMatchesPattern(toGjsonResult(val), f.Value)
		return err == nil && ok
	case spi.FilterIEq:
		return strings.EqualFold(fmt.Sprint(val), fmt.Sprint(f.Value))
	case spi.FilterINe:
		return !strings.EqualFold(fmt.Sprint(val), fmt.Sprint(f.Value))
	case spi.FilterIContains:
		return strings.Contains(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value)))
	case spi.FilterINotContains:
		return !strings.Contains(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value)))
	case spi.FilterIStartsWith:
		return strings.HasPrefix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value)))
	case spi.FilterINotStartsWith:
		return !strings.HasPrefix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value)))
	case spi.FilterIEndsWith:
		return strings.HasSuffix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value)))
	case spi.FilterINotEndsWith:
		return !strings.HasSuffix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value)))
	}
	return false
}

// extractFilterValue extracts the field value referenced by the filter.
// SourceData uses a gjson path on the entity's JSON data; SourceMeta uses
// a fixed set of metadata field names (matching the sqlite plugin's
// extractMetaValue, which is the canonical mapping for SourceMeta paths).
// Returns (value, found). found=false means the field is missing; found=true
// with value=nil means the field exists and is JSON null.
func extractFilterValue(f spi.Filter, data []byte, meta spi.EntityMeta) (any, bool) {
	if f.Source == spi.SourceMeta {
		return extractFilterMetaValue(f.Path, meta)
	}
	return extractFilterDataValue(f.Path, data)
}

func extractFilterDataValue(path string, data []byte) (any, bool) {
	result := gjson.GetBytes(data, path)
	if !result.Exists() {
		return nil, false
	}
	if result.Type == gjson.Null {
		return nil, true
	}
	return result.Value(), true
}

// extractFilterMetaValue mirrors the sqlite plugin's extractMetaValue keyset
// (plugins/sqlite/post_filter.go). Keep this list in sync with that file —
// the two must agree on which meta paths are valid for a Filter.
func extractFilterMetaValue(path string, meta spi.EntityMeta) (any, bool) {
	switch path {
	case "entity_id":
		return meta.ID, true
	case "state":
		return meta.State, true
	case "version":
		return meta.Version, true
	case "created_at":
		return timeToMicro(meta.CreationDate), true
	case "updated_at":
		return timeToMicro(meta.LastModifiedDate), true
	case "model_name":
		return meta.ModelRef.EntityName, true
	case "model_version":
		return meta.ModelRef.ModelVersion, true
	case "change_type":
		return meta.ChangeType, true
	case "transaction_id":
		return meta.TransactionID, true
	default:
		return nil, false
	}
}

// timeToMicro converts a time.Time to microseconds since Unix epoch.
// Mirrors plugins/sqlite/post_filter.go timeToMicro.
func timeToMicro(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

// compareFilterValues orders two raw values. Returns <0, 0, >0 like strings.Compare.
// Reuses operators.go toFloat64 for numeric coercion; falls back to string compare.
func compareFilterValues(a, b any) int {
	af, aerr := toFloat64(a)
	bf, berr := toFloat64(b)
	if aerr == nil && berr == nil {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

// toGjsonResult wraps a raw value in a gjson.Result for reuse of the
// existing operators.go opLike / opMatchesPattern (which take gjson.Result).
// This is a thin shim — we encode the value as JSON, parse it, and let
// gjson surface it as a Result. Used only for regex/LIKE leaf evaluation,
// where the per-entity cost is dominated by regex compile anyway.
func toGjsonResult(v any) gjson.Result {
	b, err := json.Marshal(v)
	if err != nil {
		// Fall back to a string-typed Result via fmt.Sprint.
		return gjson.Parse(fmt.Sprintf("%q", fmt.Sprint(v)))
	}
	return gjson.ParseBytes(b)
}
