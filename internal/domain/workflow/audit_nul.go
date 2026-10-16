package workflow

import (
	"reflect"
	"strings"
)

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

// valueHoldsNUL walks one value. The cases are the types the engine builds,
// kept as a fast path; the reflect fallback in the default branch is what makes
// the walk complete, so a type added later cannot leave a hole.
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
	default:
		return reflectHoldsNUL(reflect.ValueOf(v))
	}
	return false
}

// reflectHoldsNUL descends into any slice, array, map (keys and values),
// pointer or interface, and checks any value of string kind, named string
// types included. Numbers, booleans and nil cannot hold U+0000.
func reflectHoldsNUL(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.String:
		return hasNUL(rv.String())
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if reflectHoldsNUL(rv.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		for it := rv.MapRange(); it.Next(); {
			if reflectHoldsNUL(it.Key()) || reflectHoldsNUL(it.Value()) {
				return true
			}
		}
	case reflect.Pointer, reflect.Interface:
		if !rv.IsNil() {
			return reflectHoldsNUL(rv.Elem())
		}
	}
	return false
}
