package search

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ConditionToFilter translates a domain predicate.Condition into an spi.Filter.
// This is the anti-corruption layer between the domain's predicate syntax and
// the SPI's stable filter contract used by storage plugins for pushdown.
//
// fields is the model's FieldsMap (JSONPath → FieldDescriptor), used to
// classify data leaves as temporal so the resulting Filter.Coercion routes
// storage-plugin comparison correctly. A nil fields map is tolerated — every
// data leaf then stamps CoerceNone (meta temporal leaves are still classified
// via the static sortableMetaFields vocabulary regardless of fields).
func ConditionToFilter(cond predicate.Condition, fields map[string]schema.FieldDescriptor) (spi.Filter, error) {
	if cond == nil {
		return spi.Filter{}, fmt.Errorf("condition is nil")
	}

	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return simpleToFilter(c, fields)
	case *predicate.LifecycleCondition:
		return lifecycleToFilter(c), nil
	case *predicate.GroupCondition:
		return groupToFilter(c, fields)
	case *predicate.ArrayCondition:
		return arrayToFilter(c)
	case *predicate.FunctionCondition:
		return spi.Filter{}, fmt.Errorf("function conditions are not translatable to filters")
	default:
		return spi.Filter{}, fmt.Errorf("unsupported condition type: %T", cond)
	}
}

// simpleToFilter translates a SimpleCondition to a Filter with SourceData.
// Returns an error if the path cannot be represented as a pushdown filter.
func simpleToFilter(c *predicate.SimpleCondition, fields map[string]schema.FieldDescriptor) (spi.Filter, error) {
	stripped, err := stripDollarDot(c.JsonPath)
	if err != nil {
		return spi.Filter{}, err
	}
	op := mapOperator(c.OperatorType)
	return spi.Filter{
		Op:       op,
		Path:     stripped,
		Source:   spi.SourceData,
		Value:    c.Value,
		Values:   betweenValues(op, c.Value),
		Coercion: dataCoercion(c.JsonPath, fields),
	}, nil
}

// betweenValues returns the two BETWEEN bounds as a []any for downstream
// consumers that read Filter.Values (spi.evalLeafFilter/evalTemporalLeaf,
// the postgres/sqlite query planners). Every BETWEEN consumer reads Values,
// not Value — leaving Values unset makes BETWEEN silently never match.
// Returns nil for non-BETWEEN ops or a malformed (non 2-element []any)
// BETWEEN value; validation elsewhere rejects malformed BETWEEN conditions,
// and a nil Values correctly no-matches downstream rather than panicking.
func betweenValues(op spi.FilterOp, value any) []any {
	if op != spi.FilterBetween {
		return nil
	}
	vals, ok := value.([]any)
	if !ok || len(vals) != 2 {
		return nil
	}
	return vals
}

// dataCoercion returns CoerceTemporal only if the schema classifies the
// field's declared type(s) as temporal. Today classifyType never returns
// spi.OrderTemporal for data fields, so this always yields CoerceNone;
// polymorphic-temporal typing (#137) lights this up with no change here.
// A nil fields map (no schema available) also yields CoerceNone.
func dataCoercion(jsonPath string, fields map[string]schema.FieldDescriptor) spi.FilterCoercion {
	if fields == nil {
		return spi.CoerceNone
	}
	fd, ok := fields[jsonPath]
	if !ok {
		return spi.CoerceNone
	}
	if kind, err := classifyType(fd.Types); err == nil && kind == spi.OrderTemporal {
		return spi.CoerceTemporal
	}
	return spi.CoerceNone
}

// lifecycleToFilter translates a LifecycleCondition to a Filter with
// SourceMeta. The "previousTransition" alias is canonicalized to its
// storage-vocabulary name "transitionForLatestSave" (see sortableMetaFields
// in orderclass.go — the single source of truth for the meta vocabulary).
// Coercion is stamped CoerceTemporal for meta fields the vocabulary marks
// spi.OrderTemporal (currently creationDate, lastUpdateTime).
func lifecycleToFilter(c *predicate.LifecycleCondition) spi.Filter {
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}
	co := spi.CoerceNone
	if isTemporalMetaField(field) {
		co = spi.CoerceTemporal
	}
	op := mapOperator(c.OperatorType)
	return spi.Filter{
		Op:       op,
		Path:     field,
		Source:   spi.SourceMeta,
		Value:    c.Value,
		Values:   betweenValues(op, c.Value),
		Coercion: co,
	}
}

// groupToFilter translates a GroupCondition to a Filter with AND/OR children.
func groupToFilter(c *predicate.GroupCondition, fields map[string]schema.FieldDescriptor) (spi.Filter, error) {
	op := spi.FilterAnd
	if strings.EqualFold(c.Operator, "OR") {
		op = spi.FilterOr
	}
	children := make([]spi.Filter, 0, len(c.Conditions))
	for _, child := range c.Conditions {
		f, err := ConditionToFilter(child, fields)
		if err != nil {
			return spi.Filter{}, err
		}
		children = append(children, f)
	}
	return spi.Filter{Op: op, Children: children}, nil
}

// arrayToFilter translates an ArrayCondition into an AND group of positional
// equality checks. Each non-nil value in the array becomes an equality filter
// on the corresponding array index (e.g., "tags.0", "tags.2"). Nil entries
// mean "skip this position". This makes individual checks pushable to SQL
// via json_extract and correctly evaluable in post-filtering.
func arrayToFilter(c *predicate.ArrayCondition) (spi.Filter, error) {
	basePath, err := stripDollarDot(c.JsonPath)
	if err != nil {
		return spi.Filter{}, err
	}
	var children []spi.Filter
	for i, val := range c.Values {
		if val == nil {
			continue
		}
		children = append(children, spi.Filter{
			Op:     spi.FilterEq,
			Path:   fmt.Sprintf("%s.%d", basePath, i),
			Source: spi.SourceData,
			Value:  val,
		})
	}
	if len(children) == 0 {
		// All positions are nil (don't-care) — matches everything.
		// Return a tautology: an empty AND is true.
		return spi.Filter{Op: spi.FilterAnd}, nil
	}
	if len(children) == 1 {
		return children[0], nil
	}
	return spi.Filter{Op: spi.FilterAnd, Children: children}, nil
}

// stripDollarDot removes the leading "$." from a JSONPath expression and
// validates that the resulting path does not contain array-wildcard or
// advanced JSONPath syntax that cannot be pushed down to storage backends.
// Returns ("", error) when the path contains characters outside the safe
// dotted-identifier subset (letters, digits, underscore, hyphen, and dots).
// Callers fall back to in-memory filtering when this returns an error.
func stripDollarDot(path string) (string, error) {
	stripped := path
	if len(path) > 2 && path[:2] == "$." {
		stripped = path[2:]
	}
	// Reject paths containing JSONPath wildcard/array-subscript syntax
	// (e.g. "[*]", "[0]"). Such paths require in-memory evaluation and
	// cannot be translated to pushdown filters.
	for _, c := range stripped {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-', c == '.':
			// safe
		default:
			return "", fmt.Errorf("path %q contains non-pushdownable syntax (character %q)", path, c)
		}
	}
	return stripped, nil
}

// mapOperator translates a domain operator string to a spi.FilterOp.
// Unknown operators are mapped to FilterMatchesRegex to force post-filtering.
func mapOperator(op string) spi.FilterOp {
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
	case "CONTAINS":
		return spi.FilterContains
	case "STARTS_WITH":
		return spi.FilterStartsWith
	case "ENDS_WITH":
		return spi.FilterEndsWith
	case "LIKE":
		return spi.FilterLike
	case "IS_NULL":
		return spi.FilterIsNull
	case "NOT_NULL":
		return spi.FilterNotNull
	case "BETWEEN":
		return spi.FilterBetween
	case "MATCHES_PATTERN":
		return spi.FilterMatchesRegex
	case "IEQUALS":
		return spi.FilterIEq
	case "INOT_EQUAL":
		return spi.FilterINe
	case "ICONTAINS":
		return spi.FilterIContains
	case "INOT_CONTAINS":
		return spi.FilterINotContains
	case "ISTARTS_WITH":
		return spi.FilterIStartsWith
	case "INOT_STARTS_WITH":
		return spi.FilterINotStartsWith
	case "IENDS_WITH":
		return spi.FilterIEndsWith
	case "INOT_ENDS_WITH":
		return spi.FilterINotEndsWith
	default:
		return spi.FilterMatchesRegex // forces post-filter for unknown ops
	}
}
