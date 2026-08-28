package schema

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// MaxValidationDepth caps recursion in Validate to defend against stack
// exhaustion from deeply nested user-supplied documents. At roughly 8 bytes
// per nesting level a 10MB body could otherwise encode hundreds of thousands
// of levels and crash the goroutine. 256 is well above any realistic JSON
// nesting and well below the stack-blow threshold.
const MaxValidationDepth = 256

// ErrorKind classifies a ValidationError so handlers can branch on
// specific failure modes without matching error message text.
type ErrorKind int

const (
	// ErrKindGeneric covers validation failures that do not map to a
	// more specific kind (shape mismatches, malformed schema entries).
	ErrKindGeneric ErrorKind = iota

	// ErrKindUnknownElement fires when a data document carries a field
	// that the validating schema does not declare. In practice this is
	// the "stale schema" signal handlers use to decide whether to
	// refresh from authoritative storage and retry (see
	// internal/domain/entity/handler.go).
	ErrKindUnknownElement

	// ErrKindIncompatibleType fires when a leaf value's inferred DataType
	// is not assignable to any of the schema's declared DataTypes for
	// that path (e.g. submitting "abc" against an INTEGER field, or 13.111
	// against an INTEGER field that has not been widened by an extension).
	// Equivalent to Cloud's FoundIncompatibleTypeWithEntityModelException;
	// surfaces the dictionary-aligned INCOMPATIBLE_TYPE error code at the
	// HTTP boundary.
	ErrKindIncompatibleType
)

// ValidationError describes a single validation failure at a specific path.
//
// ExpectedTypes and ActualType are only populated when Kind is
// ErrKindIncompatibleType — they carry the structured context the entity
// handler renders into RFC 9457 problem-detail Props (`expectedType`,
// `actualType`).
type ValidationError struct {
	Path          string
	Message       string
	Kind          ErrorKind
	ExpectedTypes []DataType
	ActualType    DataType
}

// Error implements the error interface.
func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// HasUnknownSchemaElement reports whether any of the validation
// errors in errs classify as ErrKindUnknownElement — the stale-schema
// signal. Handlers use this to decide whether to force a cache
// refresh and re-validate once before surfacing a 4xx to the client.
func HasUnknownSchemaElement(errs []ValidationError) bool {
	for _, e := range errs {
		if e.Kind == ErrKindUnknownElement {
			return true
		}
	}
	return false
}

// FirstIncompatibleType returns a pointer to the first ErrKindIncompatibleType
// entry in errs, or nil if none is present. Handlers use this to surface the
// dictionary-aligned INCOMPATIBLE_TYPE response with structured Props
// (path, expectedType, actualType) instead of the generic BAD_REQUEST.
func FirstIncompatibleType(errs []ValidationError) *ValidationError {
	for i := range errs {
		if errs[i].Kind == ErrKindIncompatibleType {
			return &errs[i]
		}
	}
	return nil
}

// Validate checks whether data conforms to the given model schema.
// It returns a slice of validation errors; an empty slice means the data is valid.
func Validate(model *ModelNode, data any) []ValidationError {
	return validateNode(model, data, "", 0)
}

func validateNode(model *ModelNode, data any, path string, depth int) []ValidationError {
	if depth >= MaxValidationDepth {
		return []ValidationError{{
			Path:    path,
			Message: fmt.Sprintf("validation depth exceeded (max %d)", MaxValidationDepth),
			Kind:    ErrKindGeneric,
		}}
	}
	if model.Kind() == KindLeaf {
		return validateLeaf(model, data, path)
	}

	// A container node is validated against the branch the value's own kind
	// selects. A node can carry more than one: a field observed in several
	// kinds is recorded by Merge as children and/or an element and/or scalar
	// types on a single node, and every branch it carries is a kind the field
	// declares. Dispatching on the node's dominant Kind alone would refuse a
	// value the model does declare — the array branch of an object-and-array
	// union, which Merge folds onto a KindObject node.
	switch v := data.(type) {
	case nil:
		// Null against a container is admissible where the model observed one:
		// the nullable marker, and any node that also carries scalar types.
		if !model.Types().IsEmpty() {
			return nil
		}
	case map[string]any:
		if model.Kind() == KindObject {
			return validateObject(model, v, path, depth)
		}
	case []any:
		if hasArrayBranch(model) {
			return validateArray(model, v, path, depth)
		}
	default:
		if matchesScalarBranch(model, data) {
			return nil
		}
		// The scalar KIND is declared here, so the value's kind is not the
		// complaint — its type is. Answer exactly as a leaf declaration does,
		// so identical input gets the identical code and Props whether the
		// scalar was observed alone or alongside a container.
		if concrete := ConcreteTypes(model.Types()); len(concrete) > 0 {
			return []ValidationError{incompatibleType(concrete, data, path)}
		}
	}
	return []ValidationError{{
		Path:    path,
		Message: "expected " + declaredKindNames(model) + ", got " + JSONKindName(data),
		Kind:    ErrKindGeneric,
	}}
}

// hasArrayBranch reports whether node declares the array kind. An ARRAY node
// whose element was never observed — the empty-array seed the codec preserves —
// declares it just the same.
func hasArrayBranch(node *ModelNode) bool {
	return node.Kind() == KindArray || node.Element() != nil
}

// matchesScalarBranch reports whether a scalar value is assignable to one of
// the scalar types a container node carries — the record of the same field
// having been observed holding a bare scalar.
func matchesScalarBranch(node *ModelNode, data any) bool {
	dataType := inferDataType(data)
	for _, mt := range node.Types().Types() {
		if IsAssignableTo(dataType, mt) {
			return true
		}
	}
	return false
}

// declaredKindNames names the kinds a container node declares, so a rejection tells
// the caller what the field does accept rather than only what it does not.
func declaredKindNames(node *ModelNode) string {
	kinds := make([]string, 0, 3)
	if node.Kind() == KindObject {
		kinds = append(kinds, "object")
	}
	if hasArrayBranch(node) {
		kinds = append(kinds, "array")
	}
	if len(ConcreteTypes(node.Types())) > 0 {
		kinds = append(kinds, "scalar")
	}
	if len(kinds) == 0 {
		return "no value"
	}
	return strings.Join(kinds, " or ")
}

// validateObject validates the object branch of model. The caller selected it
// by the value's kind.
func validateObject(model *ModelNode, obj map[string]any, path string, depth int) []ValidationError {
	var errs []ValidationError
	children := model.Children()
	for name, childModel := range children {
		childPath := joinPath(path, name)
		val, exists := obj[name]
		if !exists {
			// Missing fields are accepted — model describes known structure, not required fields.
			continue
		}
		errs = append(errs, validateNode(childModel, val, childPath, depth+1)...)
	}
	// Extra fields in data that are not in the model are rejected.
	for name := range obj {
		if _, known := children[name]; !known {
			errs = append(errs, ValidationError{
				Path:    joinPath(path, name),
				Message: "unexpected field not present in model",
				Kind:    ErrKindUnknownElement,
			})
		}
	}
	return errs
}

// validateArray validates the array branch of model. The caller selected it by
// the value's kind.
func validateArray(model *ModelNode, arr []any, path string, depth int) []ValidationError {
	elem := model.Element()
	if elem == nil {
		return nil
	}

	var errs []ValidationError
	for i, item := range arr {
		elemPath := fmt.Sprintf("%s[%d]", path, i)
		errs = append(errs, validateNode(elem, item, elemPath, depth+1)...)
	}
	return errs
}

func validateLeaf(model *ModelNode, data any, path string) []ValidationError {
	if data == nil {
		// Null is compatible with any type.
		return nil
	}
	// Kind before type. A leaf declares a scalar and nothing else, so a
	// container value is inadmissible whatever its contents — the mirror of
	// the "expected object/array, got …" checks a container declaration
	// makes. Asking inferDataType first would classify a container as String
	// (its default for anything it does not recognise) and a STRING field
	// would then admit any array or object.
	switch data.(type) {
	case map[string]any, []any:
		return []ValidationError{{
			Path:    path,
			Message: "expected scalar, got " + JSONKindName(data),
			Kind:    ErrKindGeneric,
		}}
	}
	if matchesScalarBranch(model, data) {
		return nil
	}
	return []ValidationError{incompatibleType(model.Types().Types(), data, path)}
}

// incompatibleType builds the "kind is right, type is wrong" failure — the
// dictionary-aligned INCOMPATIBLE_TYPE signal, carrying the structured context
// the entity handler renders into problem-detail Props.
func incompatibleType(declared []DataType, data any, path string) ValidationError {
	dataType := inferDataType(data)
	// Copy declared to detach from the model node's internal slice.
	expected := make([]DataType, len(declared))
	copy(expected, declared)
	return ValidationError{
		Path:          path,
		Message:       fmt.Sprintf("value of type %s is not compatible with %v", dataType, declared),
		Kind:          ErrKindIncompatibleType,
		ExpectedTypes: expected,
		ActualType:    dataType,
	}
}

// InferDataType maps a Go value (typically from JSON decoding with
// UseNumber) to a DataType using the same classifier the walker uses.
// This ensures validation sees the same classification as schema
// inference.
func InferDataType(v any) DataType {
	return inferDataType(v)
}

// inferDataType is the internal implementation of InferDataType.
func inferDataType(v any) DataType {
	switch n := v.(type) {
	case bool:
		return Boolean
	case json.Number:
		d, err := ParseDecimal(string(n))
		if err != nil {
			// Malformed — conservatively say String (validation will fail).
			return String
		}
		stripped := d.StripTrailingZeros()
		if stripped.Scale() <= 0 {
			var bigVal *big.Int
			if stripped.Scale() == 0 {
				bigVal = stripped.Unscaled()
			} else {
				// Guard against DoS: a huge negative scale (e.g. 1e1_000_000_000)
				// would make Exp(10, -scale, nil) materialise a billion-digit big.Int.
				// Compute the approximate decimal digit count without expansion:
				//   digits = (significant digits in coefficient) + (-scale)
				// Int128 max ≈ 1.7×10^38 has 39 decimal digits; any integer needing
				// ≥ 40 digits to express is definitively UnboundInteger — skip Exp.
				const int128MaxDigits = 39
				digits := stripped.Precision() + int(-int64(stripped.Scale()))
				if digits > int128MaxDigits {
					return UnboundInteger
				}
				factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-stripped.Scale())), nil)
				bigVal = new(big.Int).Mul(stripped.Unscaled(), factor)
			}
			return ClassifyInteger(bigVal)
		}
		return ClassifyDecimal(stripped)
	case string:
		// Content-sniff ISO-8601 strings into their most specific temporal
		// subtype so a date-shaped value is classified (and later compared)
		// chronologically rather than lexically. Matches the search leaf
		// kernel's classification of stored temporal values exactly, so
		// discovery, validation, and evaluation all agree on the subtype.
		// A non-temporal string stays String (unchanged).
		if dt, ok := ClassifyTemporalString(n); ok {
			return dt
		}
		return String
	case nil:
		return Null
	default:
		// No float64/int/int64 fallbacks. Callers must use json.UseNumber.
		// If something leaks through, map to String so validation fails noisily.
		return String
	}
}

// JSONKindName names a decoded value's JSON kind in the wire vocabulary, so a
// rejection tells the caller what they sent in the terms their document is
// written in rather than in Go's type names.
func JSONKindName(data any) string {
	switch data.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		// Unreachable for json.Decoder output with UseNumber; naming the kind
		// generically keeps a leaked type out of the response.
		return "value"
	}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
