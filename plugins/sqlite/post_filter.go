package sqlite

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/tidwall/gjson"
)

// EvaluateFilter is a public wrapper around evaluateFilter exposed so that
// cross-module parity tests (e.g. against internal/match.MatchFilter) can
// pin the contract that grouped-stats / streaming-tally must produce the
// same boolean as the sqlite post-filter step for any (filter, entity)
// tuple. NOT intended for hot-path use by other code — call sites within
// this plugin should keep using evaluateFilter directly.
func EvaluateFilter(f spi.Filter, entity *spi.Entity) (bool, error) {
	return evaluateFilter(f, entity)
}

// evaluateFilter evaluates a spi.Filter against an entity's data in Go.
// This is used for post-filtering residual (non-pushable) predicates.
func evaluateFilter(f spi.Filter, entity *spi.Entity) (bool, error) {
	switch f.Op {
	case spi.FilterAnd:
		return evaluateAnd(f, entity)
	case spi.FilterOr:
		return evaluateOr(f, entity)
	default:
		return evaluateLeaf(f, entity)
	}
}

func evaluateAnd(f spi.Filter, entity *spi.Entity) (bool, error) {
	for _, child := range f.Children {
		ok, err := evaluateFilter(child, entity)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evaluateOr(f spi.Filter, entity *spi.Entity) (bool, error) {
	for _, child := range f.Children {
		ok, err := evaluateFilter(child, entity)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// extractFieldValue extracts a field value from the entity.
// SourceData uses JSON path on entity.Data; SourceMeta uses entity metadata fields.
func extractFieldValue(f spi.Filter, entity *spi.Entity) (any, bool) {
	if f.Source == spi.SourceMeta {
		return extractMetaValue(f.Path, entity)
	}
	return extractDataValue(f.Path, entity.Data)
}

// extractDataValue extracts a value from JSON data using a dot-notation path.
// Uses gjson for zero-allocation path lookups instead of full json.Unmarshal,
// which matters when post-filtering thousands of entities.
func extractDataValue(path string, data []byte) (any, bool) {
	result := gjson.GetBytes(data, path)
	if !result.Exists() {
		return nil, false
	}
	if result.Type == gjson.Null {
		return nil, true
	}
	return result.Value(), true
}

// extractMetaValue extracts a metadata field value.
func extractMetaValue(path string, entity *spi.Entity) (any, bool) {
	switch path {
	case "entity_id":
		return entity.Meta.ID, true
	case "state":
		return entity.Meta.State, true
	case "version":
		return entity.Meta.Version, true
	case "created_at":
		return timeToMicro(entity.Meta.CreationDate), true
	case "updated_at":
		return timeToMicro(entity.Meta.LastModifiedDate), true
	case "model_name":
		return entity.Meta.ModelRef.EntityName, true
	case "model_version":
		return entity.Meta.ModelRef.ModelVersion, true
	case "change_type":
		return entity.Meta.ChangeType, true
	case "transaction_id":
		return entity.Meta.TransactionID, true
	default:
		return nil, false
	}
}

func evaluateLeaf(f spi.Filter, entity *spi.Entity) (bool, error) {
	switch f.Op {
	case spi.FilterIsNull:
		_, found := extractFieldValue(f, entity)
		return !found, nil
	case spi.FilterNotNull:
		val, found := extractFieldValue(f, entity)
		return found && val != nil, nil
	}

	val, found := extractFieldValue(f, entity)

	switch f.Op {
	case spi.FilterEq:
		if !found || val == nil {
			return false, nil
		}
		return compareValues(val, f.Value) == 0, nil
	case spi.FilterNe:
		if !found || val == nil {
			return true, nil
		}
		return compareValues(val, f.Value) != 0, nil
	case spi.FilterGt:
		if !found || val == nil {
			return false, nil
		}
		return compareValues(val, f.Value) > 0, nil
	case spi.FilterLt:
		if !found || val == nil {
			return false, nil
		}
		return compareValues(val, f.Value) < 0, nil
	case spi.FilterGte:
		if !found || val == nil {
			return false, nil
		}
		return compareValues(val, f.Value) >= 0, nil
	case spi.FilterLte:
		if !found || val == nil {
			return false, nil
		}
		return compareValues(val, f.Value) <= 0, nil
	case spi.FilterContains:
		if !found || val == nil {
			return false, nil
		}
		return strings.Contains(fmt.Sprint(val), fmt.Sprint(f.Value)), nil
	case spi.FilterStartsWith:
		if !found || val == nil {
			return false, nil
		}
		return strings.HasPrefix(fmt.Sprint(val), fmt.Sprint(f.Value)), nil
	case spi.FilterEndsWith:
		if !found || val == nil {
			return false, nil
		}
		return strings.HasSuffix(fmt.Sprint(val), fmt.Sprint(f.Value)), nil
	case spi.FilterLike:
		if !found || val == nil {
			return false, nil
		}
		return matchLike(fmt.Sprint(val), fmt.Sprint(f.Value)), nil
	case spi.FilterBetween:
		if !found || val == nil {
			return false, nil
		}
		if len(f.Values) < 2 {
			return false, nil
		}
		return compareValues(val, f.Values[0]) >= 0 && compareValues(val, f.Values[1]) <= 0, nil
	case spi.FilterMatchesRegex:
		if !found || val == nil {
			return false, nil
		}
		re, err := regexp.Compile(fmt.Sprint(f.Value))
		if err != nil {
			return false, fmt.Errorf("invalid regex %q: %w", f.Value, err)
		}
		return re.MatchString(fmt.Sprint(val)), nil
	case spi.FilterIEq:
		if !found || val == nil {
			return false, nil
		}
		return strings.EqualFold(fmt.Sprint(val), fmt.Sprint(f.Value)), nil
	case spi.FilterINe:
		if !found || val == nil {
			return true, nil
		}
		return !strings.EqualFold(fmt.Sprint(val), fmt.Sprint(f.Value)), nil
	case spi.FilterIContains:
		if !found || val == nil {
			return false, nil
		}
		return strings.Contains(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value))), nil
	case spi.FilterINotContains:
		if !found || val == nil {
			return true, nil
		}
		return !strings.Contains(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value))), nil
	case spi.FilterIStartsWith:
		if !found || val == nil {
			return false, nil
		}
		return strings.HasPrefix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value))), nil
	case spi.FilterINotStartsWith:
		if !found || val == nil {
			return true, nil
		}
		return !strings.HasPrefix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value))), nil
	case spi.FilterIEndsWith:
		if !found || val == nil {
			return false, nil
		}
		return strings.HasSuffix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value))), nil
	case spi.FilterINotEndsWith:
		if !found || val == nil {
			return true, nil
		}
		return !strings.HasSuffix(strings.ToLower(fmt.Sprint(val)), strings.ToLower(fmt.Sprint(f.Value))), nil
	default:
		return false, fmt.Errorf("unsupported filter op: %s", f.Op)
	}
}

// compareValues compares two values for ordering.
// Returns <0 if a < b, 0 if a == b, >0 if a > b.
func compareValues(a, b any) int {
	af := toFloat64(a)
	bf := toFloat64(b)
	if af != nil && bf != nil {
		switch {
		case *af < *bf:
			return -1
		case *af > *bf:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

// toFloat64 attempts to convert a value to float64.
func toFloat64(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

// matchLike implements SQL LIKE pattern matching in Go.
// % matches any sequence, _ matches any single byte.
// Backslash is the escape character.
//
// matchLike operates on bytes, not runes. This matches SQLite's default LIKE
// behavior where _ matches a single byte, not a Unicode code point.
func matchLike(s, pattern string) bool {
	return matchLikeHelper(s, 0, pattern, 0)
}

func matchLikeHelper(s string, si int, pattern string, pi int) bool {
	for pi < len(pattern) {
		ch := pattern[pi]
		switch {
		case ch == '\\' && pi+1 < len(pattern):
			// Escaped character — match literally.
			pi++
			if si >= len(s) || s[si] != pattern[pi] {
				return false
			}
			si++
			pi++
		case ch == '%':
			// Skip consecutive %s.
			for pi < len(pattern) && pattern[pi] == '%' {
				pi++
			}
			if pi == len(pattern) {
				return true
			}
			// Try matching the rest starting at each position.
			for si <= len(s) {
				if matchLikeHelper(s, si, pattern, pi) {
					return true
				}
				si++
			}
			return false
		case ch == '_':
			if si >= len(s) {
				return false
			}
			si++
			pi++
		default:
			if si >= len(s) || s[si] != ch {
				return false
			}
			si++
			pi++
		}
	}
	return si == len(s)
}
