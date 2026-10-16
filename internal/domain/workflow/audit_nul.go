package workflow

import "strings"

// holdsNUL reports whether an audit event's text holds U+0000: the state, the
// details, and every key and string value in data at any depth. PostgreSQL
// refuses U+0000 in jsonb and the refused INSERT aborts the request's
// transaction, so recordEvent refuses such an event before any backend sees it.
func holdsNUL(state, details string, data map[string]any) bool {
	return hasNUL(state) || hasNUL(details) || mapHoldsNUL(data)
}

func hasNUL(s string) bool { return strings.ContainsRune(s, 0) }

func mapHoldsNUL(m map[string]any) bool {
	for k, v := range m {
		if hasNUL(k) || valueHoldsNUL(v) {
			return true
		}
	}
	return false
}

func valueHoldsNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return hasNUL(x)
	case map[string]any:
		return mapHoldsNUL(x)
	case []any:
		for _, e := range x {
			if valueHoldsNUL(e) {
				return true
			}
		}
	case []string:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case []map[string]any:
		for _, e := range x {
			if mapHoldsNUL(e) {
				return true
			}
		}
	}
	return false
}
