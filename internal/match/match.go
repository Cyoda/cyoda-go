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

// FieldTypes resolves a condition's JSONPath (in "$."-prefixed FieldsMap key
// form, with "[*]" marking array-wildcard hops) to the field's declared model
// DataTypes. It is the plumbing that makes the predicate-tree evaluator
// type-directed, exactly like the search pushdown: a temporal data field whose
// stored JSON is an ISO string is only known to compare temporally (not
// lexically) because the model declares its type here.
//
// A nil FieldTypes, or a lookup that returns nil for an unknown/untyped path,
// is tolerated: comparison/range leaves on that path degrade to non-match
// (they cannot be typed), while string and null-test leaves are unaffected.
type FieldTypes func(jsonPath string) []spi.DataType

// Match evaluates a predicate.Condition against entity data and metadata,
// returning true if the entity satisfies the condition. fieldTypes supplies
// the declared model types for data leaves; pass nil when no model is
// available (comparison leaves then non-match, string/null leaves still work).
func Match(condition predicate.Condition, entityData []byte, entityMeta spi.EntityMeta, fieldTypes FieldTypes) (bool, error) {
	switch c := condition.(type) {
	case *predicate.SimpleCondition:
		return matchSimple(c, entityData, fieldTypes)
	case *predicate.LifecycleCondition:
		return matchLifecycle(c, entityMeta)
	case *predicate.GroupCondition:
		return matchGroup(c, entityData, entityMeta, fieldTypes)
	case *predicate.ArrayCondition:
		return matchArray(c, entityData, fieldTypes)
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

// arrayElementFieldPath returns the FieldsMap key that addresses an
// ArrayCondition's element type. ArrayCondition names a container path
// ("$.tags"); the model records the element type under the same path with a
// trailing "[*]" ("$.tags[*]"), mirroring search.arrayElementPath so the
// predicate evaluator and the search pushdown stamp the same declared types on
// positional array leaves.
func arrayElementFieldPath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "$") {
		p = "$." + p
	}
	if strings.HasSuffix(p, "[*]") {
		return p
	}
	return p + "[*]"
}

func matchSimple(c *predicate.SimpleCondition, data []byte, fieldTypes FieldTypes) (bool, error) {
	path := convertJSONPath(c.JsonPath)
	result := gjson.GetBytes(data, path)

	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(c.JsonPath)
	}

	// If the path produced an array result (from # wildcard), check if ANY
	// element matches for applicable operators.
	if result.IsArray() {
		return matchArrayWildcard(c.OperatorType, result, c.Value, declared)
	}

	return applyOperator(c.OperatorType, result, c.Value, declared)
}

// matchArrayWildcard checks if any element in an array result matches the
// operator. declared is the array element's declared type set (the wildcard
// path is itself the FieldsMap key, e.g. "$.laureates[*].motivation").
func matchArrayWildcard(operatorType string, arrayResult gjson.Result, expected any, declared []spi.DataType) (bool, error) {
	var lastErr error
	matched := false

	arrayResult.ForEach(func(_, value gjson.Result) bool {
		ok, err := applyOperator(operatorType, value, expected, declared)
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
// compare chronologically via the temporal kernel branch (declared
// ZonedDateTime) regardless of operator, and the remaining canonical meta
// fields compare as declared strings via the same kernel. See spec §6.3-§6.5
// (temporal-search-filters design) for the canonical vocabulary and the
// rationale for unconditional field-identity routing.
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

// applyStringLifecycle evaluates a string-valued meta field: wrap the value in
// a gjson document and route it through the kernel with a declared String
// type, so meta string comparison shares the one comparison core with data
// leaves and the search pushdown.
func applyStringLifecycle(c *predicate.LifecycleCondition, value string) (bool, error) {
	fakeJSON := fmt.Sprintf(`{"v":%q}`, value)
	result := gjson.Get(fakeJSON, "v")
	return applyOperator(c.OperatorType, result, c.Value, []spi.DataType{spi.String})
}

// matchTemporalMeta compares a stored meta time.Time chronologically against
// the condition operand(s) via the kernel. The stored instant is bridged to a
// gjson.Result (an RFC3339 string from json.Marshal) and evaluated with a
// declared ZonedDateTime type, so meta temporal comparison shares the single
// EvalLeaf kernel with data-field temporal comparison. A zero-value time
// (unset) bridges to an absent Result: IS_NULL matches, every binary op
// (including NOT_EQUAL) non-matches under the kernel's null uniformity.
func matchTemporalMeta(op string, stored time.Time, value any) (bool, error) {
	// A temporal field admits only comparison / range / null operators. A
	// non-comparison operator (e.g. CONTAINS) is invalid on a temporal field
	// and degrades to non-match here — it must NOT lexically substring-match the
	// formatted RFC3339 string. Validated entry points reject these operators up
	// front; this guard keeps the unvalidated in-process path safe.
	if !isTemporalOperator(op) {
		return false, nil
	}
	var result gjson.Result
	if !stored.IsZero() {
		b, err := json.Marshal(stored)
		if err != nil {
			return false, nil
		}
		result = gjson.ParseBytes(b)
	}
	return applyOperator(op, result, value, []spi.DataType{spi.ZonedDateTime})
}

// isTemporalOperator reports whether op is a valid operator on a temporal
// field: the six comparisons, the two range ops, and the two null tests.
// String operators are excluded so a temporal field never substring-matches
// its formatted representation.
func isTemporalOperator(op string) bool {
	switch op {
	case "EQUALS", "NOT_EQUAL",
		"GREATER_THAN", "LESS_THAN", "GREATER_OR_EQUAL", "LESS_OR_EQUAL",
		"BETWEEN", "BETWEEN_INCLUSIVE",
		"IS_NULL", "NOT_NULL":
		return true
	default:
		return false
	}
}

func matchGroup(c *predicate.GroupCondition, data []byte, meta spi.EntityMeta, fieldTypes FieldTypes) (bool, error) {
	switch c.Operator {
	case "AND":
		for _, child := range c.Conditions {
			ok, err := Match(child, data, meta, fieldTypes)
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
			ok, err := Match(child, data, meta, fieldTypes)
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

func matchArray(c *predicate.ArrayCondition, data []byte, fieldTypes FieldTypes) (bool, error) {
	basePath := convertJSONPath(c.JsonPath)

	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(arrayElementFieldPath(c.JsonPath))
	}

	for i, expected := range c.Values {
		if expected == nil {
			continue // skip null positions
		}

		elemPath := fmt.Sprintf("%s.%d", basePath, i)
		result := gjson.GetBytes(data, elemPath)

		// Each positional value is an equality check on the array element,
		// routed through the kernel so numeric/type-directed semantics match
		// scalar EQUALS and the search pushdown's arrayToFilter. A missing
		// element (absent Result) non-matches under the kernel's null rule.
		ok, err := applyOperator("EQUALS", result, expected, declared)
		if err != nil || !ok {
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
