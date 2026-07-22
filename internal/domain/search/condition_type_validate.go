package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// skipTypeCheckOperators lists operators whose condition value is not compared
// against the field's DataType. IS_NULL and NOT_NULL don't use the value
// semantically; the value is always null and any type is acceptable.
var skipTypeCheckOperators = map[string]struct{}{
	"IS_NULL":  {},
	"NOT_NULL": {},
}

// ValidateConditionValueTypes walks a condition tree and checks that each
// simple clause's value is type-compatible with the field's DataType as
// declared in the model schema.
//
// The model's FieldsMap provides a lookup from JSONPath (e.g. "$.price") to
// a FieldDescriptor carrying the observed DataType(s). Conditions referencing
// unknown paths are accepted (the condition may traverse a path not yet seen
// in training data).
//
// Returns a non-nil error if any simple clause has a type-mismatched value.
// Polymorphic fields (>1 type) accept values matching any participating type.
// Null values are accepted for any field type.
func ValidateConditionValueTypes(model *schema.ModelNode, cond predicate.Condition) error {
	if model == nil || cond == nil {
		return nil
	}
	fm := model.FieldsMap()
	return walkConditionTypes(fm, cond, 0)
}

func walkConditionTypes(fm map[string]schema.FieldDescriptor, cond predicate.Condition, depth int) error {
	if cond == nil {
		return nil
	}
	if depth >= MaxConditionDepth {
		return fmt.Errorf("condition depth exceeded (max %d)", MaxConditionDepth)
	}
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return validateSimpleConditionType(fm, c)
	case *predicate.GroupCondition:
		for _, child := range c.Conditions {
			if err := walkConditionTypes(fm, child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.LifecycleCondition:
		return validateLifecycleType(c)
	case *predicate.ArrayCondition, *predicate.FunctionCondition:
		return nil
	default:
		return nil
	}
}

func validateSimpleConditionType(fm map[string]schema.FieldDescriptor, c *predicate.SimpleCondition) error {
	// Operators that don't perform value comparison bypass type checking.
	if _, skip := skipTypeCheckOperators[c.OperatorType]; skip {
		return nil
	}

	// Null values are compatible with any field type.
	if c.Value == nil {
		return nil
	}

	fd, ok := fm[c.JsonPath]
	if !ok {
		// Unknown path — no type constraint; accept.
		return nil
	}

	if len(fd.Types) == 0 {
		// No type information recorded — accept.
		return nil
	}

	// Branch on composite vs scalar values before calling inferValueDataType.
	switch v := c.Value.(type) {
	case []any:
		// Array values (e.g. BETWEEN [lo, hi], IN [a, b, c]): every element
		// must type-check against the field. An empty array is accepted (no
		// elements means nothing to mismatch).
		for i, elem := range v {
			if err := checkSingleValueType(fd, c.JsonPath, elem); err != nil {
				return fmt.Errorf("value[%d]: %w", i, err)
			}
		}
		return nil
	case map[string]any:
		// No search operator accepts an object value.
		_ = v
		return fmt.Errorf("condition value for field %q is an object, which is not valid for any operator type: %w",
			c.JsonPath, errConditionTypeMismatch)
	default:
		return checkSingleValueType(fd, c.JsonPath, c.Value)
	}
}

// errConditionTypeMismatch is the sentinel error for condition type mismatch.
// Handlers check errors.Is(err, errConditionTypeMismatch) to emit HTTP 400
// with ErrCodeConditionTypeMismatch.
var errConditionTypeMismatch = fmt.Errorf("condition type mismatch")

// errInvalidFieldPath is the sentinel error for a condition referencing a
// meta field path the vocabulary does not recognize. Handlers check
// errors.Is(err, errInvalidFieldPath) to emit HTTP 400 with
// ErrCodeInvalidFieldPath (distinct from errConditionTypeMismatch's
// CONDITION_TYPE_MISMATCH: the field itself is unknown, not merely
// type-incompatible with its operator/operand).
var errInvalidFieldPath = fmt.Errorf("invalid field path")

// comparisonOps is the operator family valid against temporal meta fields:
// ordering, equality, BETWEEN, and the null-presence checks. String-shaped
// operators (CONTAINS, STARTS_WITH, LIKE, regex, case-insensitive variants,
// ...) have no meaningful temporal semantics and are rejected.
var comparisonOps = map[string]bool{
	"EQUALS": true, "NOT_EQUAL": true, "GREATER_THAN": true, "LESS_THAN": true,
	"GREATER_OR_EQUAL": true, "LESS_OR_EQUAL": true, "BETWEEN": true,
	"IS_NULL": true, "NOT_NULL": true,
}

// validateLifecycleType enforces type-soundness for LifecycleCondition
// (meta) clauses:
//   - the field must be a known meta filter field (sortableMetaFields key,
//     or the previousTransition alias) — otherwise errInvalidFieldPath.
//   - for fields the meta vocabulary classifies as temporal (creationDate,
//     lastUpdateTime), the operator must be one of comparisonOps and every
//     operand (skipped for IS_NULL/NOT_NULL) must be an offset-bearing
//     RFC3339 timestamp per spi.ParseTemporalMillis — otherwise
//     errConditionTypeMismatch.
//
// Non-temporal meta fields (state, transitionForLatestSave, transactionId,
// id) carry no further constraint here: they compare as their stored
// text/string form regardless of operator.
func validateLifecycleType(c *predicate.LifecycleCondition) error {
	if !isKnownMetaFilterField(c.Field) {
		return fmt.Errorf("unknown meta filter field %q: %w", c.Field, errInvalidFieldPath)
	}
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}
	if !isTemporalMetaField(field) {
		return nil
	}
	if !comparisonOps[c.OperatorType] {
		return fmt.Errorf("operator %q is not valid on temporal field %q: %w", c.OperatorType, c.Field, errConditionTypeMismatch)
	}
	if c.OperatorType == "IS_NULL" || c.OperatorType == "NOT_NULL" {
		return nil
	}
	for _, v := range operandStrings(c.Value) {
		if _, ok := spi.ParseTemporalMillis(v); !ok {
			return fmt.Errorf("operand %q is not a valid timestamp for temporal field %q: %w", v, c.Field, errConditionTypeMismatch)
		}
	}
	return nil
}

// operandStrings normalizes a condition value into its comparable operand
// strings: a scalar becomes a single-element slice; a []any (BETWEEN's
// [lo, hi] pair) becomes one element per member, each stringified via
// fmt.Sprint to match the formatting evalTemporalLeaf/spi.ParseTemporalMillis
// callers use elsewhere in the temporal pipeline.
func operandStrings(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, elem := range arr {
			out = append(out, fmt.Sprint(elem))
		}
		return out
	}
	return []string{fmt.Sprint(v)}
}

// checkSingleValueType checks whether a single scalar value is compatible with
// the field's TypeSet. Null values are accepted for any field type. String-only
// fields accept any value type (lexicographic comparison semantics).
func checkSingleValueType(fd schema.FieldDescriptor, jsonPath string, v any) error {
	if v == nil {
		return nil // null compatible with any type
	}

	valueType := inferValueDataType(v)
	if valueType == schema.Null {
		return nil // null compatible with any type
	}

	// Only enforce type compatibility when the field carries at least one
	// numeric or boolean type. String fields accept any comparison value
	// (numeric or string) to support lexicographic and coerced comparisons.
	// This matches the Cloud's InvalidTypesInClientConditionException semantics,
	// which targets "non-string value against a non-string field" mismatches.
	hasConstrainedType := false
	for _, ft := range fd.Types {
		if schema.IsNumeric(ft) || ft == schema.Boolean {
			hasConstrainedType = true
			break
		}
	}
	if !hasConstrainedType {
		return nil
	}

	for _, fieldType := range fd.Types {
		if schema.IsAssignableTo(valueType, fieldType) {
			return nil
		}
	}

	return fmt.Errorf("condition value type %s is not compatible with field %q (expected %v): %w",
		valueType, jsonPath, fd.Types, errConditionTypeMismatch)
}

// inferValueDataType infers the DataType of a condition value.
//
// Condition values come from predicate.ParseCondition which uses standard
// json.Unmarshal — numbers arrive as float64, not json.Number. We convert
// float64 to json.Number so the schema classifier can apply its full
// precision-based widening lattice, rather than defaulting to String.
func inferValueDataType(v any) schema.DataType {
	switch val := v.(type) {
	case []any, map[string]any:
		// Composite values (e.g. BETWEEN [lo, hi]) — skip type check.
		return schema.Null
	case float64:
		// Standard json.Unmarshal produces float64. Convert to json.Number
		// so InferDataType can classify it correctly.
		return schema.InferDataType(json.Number(strconv.FormatFloat(val, 'f', -1, 64)))
	}
	return schema.InferDataType(v)
}

// loadModelNode fetches and parses the model schema for ref, returning the
// *schema.ModelNode used for condition-type validation. Returns nil when
// the store lookup fails, the descriptor has no schema bound, or the schema
// fails to parse — callers treat this as "no type constraints available"
// rather than failing the search on a schema-load hiccup. EnsureModelRegistered
// has already confirmed the model exists by the time this runs, so in the
// normal case the node is present.
func loadModelNode(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) *schema.ModelNode {
	desc, err := store.Get(ctx, ref)
	if err != nil || desc == nil || len(desc.Schema) == 0 {
		return nil
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil
	}
	return node
}

// validateConditionTypes is the single boundary enforcing condition
// type-soundness for every SearchService entry point (HTTP, gRPC, and any
// future transport funnel through Search/SubmitAsync). It loads the model
// schema and delegates to ValidateConditionValueTypes, mapping the returned
// sentinel error to the appropriate 400-classified *common.AppError:
// errInvalidFieldPath → INVALID_FIELD_PATH (the field itself is unknown),
// anything else → CONDITION_TYPE_MISMATCH (the value is type-incompatible
// with a known field/operator).
//
// A schema-load hiccup (see loadModelNode) returns nil — the search
// proceeds without type constraints rather than failing on infra flakiness.
func (s *SearchService) validateConditionTypes(ctx context.Context, modelStore spi.ModelStore, modelRef spi.ModelRef, cond predicate.Condition) *common.AppError {
	node := loadModelNode(ctx, modelStore, modelRef)
	if node == nil {
		return nil
	}
	if err := ValidateConditionValueTypes(node, cond); err != nil {
		code := common.ErrCodeConditionTypeMismatch
		if errors.Is(err, errInvalidFieldPath) {
			code = common.ErrCodeInvalidFieldPath
		}
		return common.Operational(http.StatusBadRequest, code, err.Error())
	}
	return nil
}
