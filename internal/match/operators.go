package match

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// applyOperator dispatches to the appropriate operator function.
func applyOperator(operatorType string, actual gjson.Result, expected any) (bool, error) {
	switch operatorType {
	// Equality
	case "EQUALS":
		return opEquals(actual, expected), nil
	case "NOT_EQUAL":
		return !opEquals(actual, expected), nil
	case "IS_NULL":
		return opIsNull(actual), nil
	case "NOT_NULL":
		return !opIsNull(actual), nil

	// Comparison
	case "GREATER_THAN":
		return opCompare(actual, expected, func(a, b float64) bool { return a > b }, func(a, b string) bool { return a > b })
	case "LESS_THAN":
		return opCompare(actual, expected, func(a, b float64) bool { return a < b }, func(a, b string) bool { return a < b })
	case "GREATER_OR_EQUAL":
		return opCompare(actual, expected, func(a, b float64) bool { return a >= b }, func(a, b string) bool { return a >= b })
	case "LESS_OR_EQUAL":
		return opCompare(actual, expected, func(a, b float64) bool { return a <= b }, func(a, b string) bool { return a <= b })
	case "BETWEEN", "BETWEEN_INCLUSIVE":
		return opBetween(actual, expected)

	// String
	case "CONTAINS":
		return opContains(actual, expected, false), nil
	case "NOT_CONTAINS":
		return !opContains(actual, expected, false), nil
	case "STARTS_WITH":
		return opStartsWith(actual, expected, false), nil
	case "NOT_STARTS_WITH":
		return !opStartsWith(actual, expected, false), nil
	case "ENDS_WITH":
		return opEndsWith(actual, expected, false), nil
	case "NOT_ENDS_WITH":
		return !opEndsWith(actual, expected, false), nil
	case "MATCHES_PATTERN":
		return opMatchesPattern(actual, expected)
	case "LIKE":
		return opLike(actual, expected)

	// Case-insensitive
	case "IEQUALS":
		return opIEquals(actual, expected), nil
	case "INOT_EQUAL":
		return !opIEquals(actual, expected), nil
	case "ICONTAINS":
		return opContains(actual, expected, true), nil
	case "INOT_CONTAINS":
		return !opContains(actual, expected, true), nil
	case "ISTARTS_WITH":
		return opStartsWith(actual, expected, true), nil
	case "INOT_STARTS_WITH":
		return !opStartsWith(actual, expected, true), nil
	case "IENDS_WITH":
		return opEndsWith(actual, expected, true), nil
	case "INOT_ENDS_WITH":
		return !opEndsWith(actual, expected, true), nil

	// Not implemented
	case "IS_CHANGED", "IS_UNCHANGED":
		return false, fmt.Errorf("operator %s not implemented", operatorType)

	default:
		return false, fmt.Errorf("unknown operator: %s", operatorType)
	}
}

func opIsNull(actual gjson.Result) bool {
	return !actual.Exists() || actual.Type == gjson.Null
}

func opEquals(actual gjson.Result, expected any) bool {
	if opIsNull(actual) {
		return expected == nil
	}

	expStr := fmt.Sprintf("%v", expected)

	// Try numeric comparison if both are numbers. spi.NumericFloat
	// deliberately does not parse strings (unlike toFloat64) — a
	// string-encoded numeric operand falls through to the lexical
	// comparison below, aligning with spi.compareFilterValues.
	if actual.Type == gjson.Number {
		if expFloat, ok := spi.NumericFloat(expected); ok {
			return actual.Float() == expFloat
		}
	}

	return actual.String() == expStr
}

func opIEquals(actual gjson.Result, expected any) bool {
	if opIsNull(actual) {
		return expected == nil
	}
	expStr := fmt.Sprintf("%v", expected)
	return strings.EqualFold(actual.String(), expStr)
}

func opCompare(actual gjson.Result, expected any,
	numCmp func(float64, float64) bool,
	strCmp func(string, string) bool,
) (bool, error) {
	if opIsNull(actual) {
		return false, nil
	}

	// spi.NumericFloat does not parse strings — a string operand (even a
	// numeric-looking one) falls through to the lexical strCmp branch below,
	// aligning with spi.compareFilterValues (the pushdown evaluator).
	if expFloat, ok := spi.NumericFloat(expected); ok && actual.Type == gjson.Number {
		return numCmp(actual.Float(), expFloat), nil
	}

	expStr := fmt.Sprintf("%v", expected)
	return strCmp(actual.String(), expStr), nil
}

func opBetween(actual gjson.Result, expected any) (bool, error) {
	if opIsNull(actual) {
		return false, nil
	}

	var lo, hi float64
	switch v := expected.(type) {
	case string:
		parts := strings.SplitN(v, ",", 2)
		if len(parts) != 2 {
			return false, fmt.Errorf("BETWEEN expects two values separated by comma, got: %s", v)
		}
		var err error
		lo, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			return false, fmt.Errorf("BETWEEN: invalid lower bound: %w", err)
		}
		hi, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return false, fmt.Errorf("BETWEEN: invalid upper bound: %w", err)
		}
	case []any:
		if len(v) != 2 {
			return false, fmt.Errorf("BETWEEN expects exactly 2 values, got %d", len(v))
		}
		var err error
		lo, err = toFloat64(v[0])
		if err != nil {
			return false, fmt.Errorf("BETWEEN: invalid lower bound: %w", err)
		}
		hi, err = toFloat64(v[1])
		if err != nil {
			return false, fmt.Errorf("BETWEEN: invalid upper bound: %w", err)
		}
	default:
		return false, fmt.Errorf("BETWEEN: unsupported value type %T", expected)
	}

	val := actual.Float()
	return val >= lo && val <= hi, nil
}

func opContains(actual gjson.Result, expected any, caseInsensitive bool) bool {
	if opIsNull(actual) {
		return false
	}
	expStr := fmt.Sprintf("%v", expected)
	actualStr := actual.String()
	if caseInsensitive {
		actualStr = strings.ToLower(actualStr)
		expStr = strings.ToLower(expStr)
	}
	return strings.Contains(actualStr, expStr)
}

func opStartsWith(actual gjson.Result, expected any, caseInsensitive bool) bool {
	if opIsNull(actual) {
		return false
	}
	expStr := fmt.Sprintf("%v", expected)
	actualStr := actual.String()
	if caseInsensitive {
		actualStr = strings.ToLower(actualStr)
		expStr = strings.ToLower(expStr)
	}
	return strings.HasPrefix(actualStr, expStr)
}

func opEndsWith(actual gjson.Result, expected any, caseInsensitive bool) bool {
	if opIsNull(actual) {
		return false
	}
	expStr := fmt.Sprintf("%v", expected)
	actualStr := actual.String()
	if caseInsensitive {
		actualStr = strings.ToLower(actualStr)
		expStr = strings.ToLower(expStr)
	}
	return strings.HasSuffix(actualStr, expStr)
}

func opMatchesPattern(actual gjson.Result, expected any) (bool, error) {
	if opIsNull(actual) {
		return false, nil
	}
	pattern := fmt.Sprintf("%v", expected)
	return regexp.MatchString(pattern, actual.String())
}

func opLike(actual gjson.Result, expected any) (bool, error) {
	if opIsNull(actual) {
		return false, nil
	}
	pattern := fmt.Sprintf("%v", expected)

	// Convert SQL LIKE to regex by processing character-by-character.
	// We must handle % and _ before escaping other regex metacharacters.
	var b strings.Builder
	b.WriteString("^")
	for _, ch := range pattern {
		switch ch {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")

	return regexp.MatchString(b.String(), actual.String())
}

// toFloat64 converts various numeric types and strings to float64.
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}
