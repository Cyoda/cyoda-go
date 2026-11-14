package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// MaxValidationDepth caps recursion in Validate to defend against stack
// exhaustion from deeply nested user-supplied documents. At roughly 8 bytes
// per nesting level a 10MB body could otherwise encode hundreds of thousands
// of levels and crash the goroutine. 256 is well above any realistic JSON
// nesting and well below the stack-blow threshold.
const MaxValidationDepth = 256

// depthExceededMessage is the message half of DepthExceededError's wire
// text, shared with Validate's rendering of it. Computed from
// MaxValidationDepth rather than duplicated as a literal, so the two can
// never drift out of sync if the cap ever changes.
var depthExceededMessage = fmt.Sprintf("validation depth exceeded (max %d)", MaxValidationDepth)

// ErrorKind classifies a ValidationError so handlers can branch on
// specific failure modes without matching error message text. The kinds a
// handler branches on are ErrKindIncompatibleType (see FirstIncompatibleType)
// and, by exclusion, ErrKindGeneric.
type ErrorKind int

const (
	// ErrKindGeneric covers every validation failure that is not an
	// incompatible leaf type: unknown fields, shape mismatches and
	// malformed schema entries.
	ErrKindGeneric ErrorKind = iota

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

// Validate checks whether data conforms to the model without extending it.
// It returns a slice of validation errors; an empty slice means the data is
// valid.
//
// This asks Admit's own traversal the same question Extend asks and renders
// every change as a refusal, so strict validation and the change-level gate
// cannot disagree about what a field accepts. It builds the admitter
// directly (rather than calling the public Admit) with continueOnInvalidName
// set: strict validation never establishes a field
// (docs/cloud-parity/model-field-name-grammar.md), so an unspellable field
// name is not the grammar-violation Admit/Extend refuse outright — it is
// just another field the stored model does not declare, ReasonNewField's
// ordinary unknown-field case renders identically — and, unlike Extend,
// finding one must not abort the rest of the document.
func Validate(model *ModelNode, data any) []ValidationError {
	a := &admitter{continueOnInvalidName: true}
	_, err := a.node(model, data, "", "", 0, spi.ChangeLevelType)
	changes := a.changes
	if err != nil {
		if ve, ok := depthExceededValidationError(err); ok {
			return []ValidationError{ve}
		}
		if ve, ok := invalidFieldNameValidationError(err); ok {
			return []ValidationError{ve}
		}
		if ve, ok := unsupportedValueValidationError(err); ok {
			return []ValidationError{ve}
		}
		return []ValidationError{{Message: "unsupported value", Kind: ErrKindGeneric}}
	}
	errs := make([]ValidationError, 0, len(changes))
	for _, c := range changes {
		errs = append(errs, validationErrorFor(c))
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// depthExceededValidationError recognises Admit's *DepthExceededError and
// renders it into the ValidationError shape this package has always used
// for the failure: Path and Message as separate fields, not the path folded
// into the message text. errors.As, not string matching — a generic
// {Message: err.Error()} would have doubled the path into the
// ValidationError.Error() rendering (which already prefixes Path), dropped
// the machine-readable Path field, and silently degraded the moment
// DepthExceededError's wording drifted from whatever a string match
// expected.
func depthExceededValidationError(err error) (ValidationError, bool) {
	var de *DepthExceededError
	if !errors.As(err, &de) {
		return ValidationError{}, false
	}
	return ValidationError{
		Path:    wirePath(de.Path),
		Message: depthExceededMessage,
		Kind:    ErrKindGeneric,
	}, true
}

// invalidFieldNameValidationError recognises *InvalidFieldNameError as a
// backstop (re-review finding #2): under continueOnInvalidName, an
// unspellable nested name is ordinarily captured as a ReasonInvalidFieldName
// Change and never reaches here as a raw error — describeAt escalates it
// from its own throwaway admitter up the call chain. This exists for
// whatever path does not go through that escalation (a future call site
// that forgets it, or a caller that reaches node/object/array directly with
// continueOnInvalidName unset while still wanting Validate's rendering): a
// bad name is not a Go-internals leak the generic "unsupported value"
// fallback is for, it is exactly the unknown-field error
// strict validation renders it as everywhere else — so it gets that
// rendering here too, not a worse one.
func invalidFieldNameValidationError(err error) (ValidationError, bool) {
	var fe *InvalidFieldNameError
	if !errors.As(err, &fe) {
		return ValidationError{}, false
	}
	return ValidationError{
		Path:    wirePath(fe.Path),
		Message: "unexpected field not present in model",
		Kind:    ErrKindGeneric,
	}, true
}

// unsupportedValueValidationError recognises Admit's *UnsupportedValueError
// (final review M4) and renders it with a fixed, client-safe message: the
// case is a caller-side contract violation (a raw float64 leaking through
// without json.UseNumber decoding, or a Go type node never produces at all),
// and its Kind field exists for logs/callers, not for echoing a %T-formatted
// Go type name into a client-facing ValidationError.Message.
func unsupportedValueValidationError(err error) (ValidationError, bool) {
	var ue *UnsupportedValueError
	if !errors.As(err, &ue) {
		return ValidationError{}, false
	}
	return ValidationError{
		Path:    wirePath(ue.Path),
		Message: "unsupported value",
		Kind:    ErrKindGeneric,
	}, true
}

// wirePath adapts Admit's path convention onto the wire shape Validate's
// ValidationError.Path has always used.
//
// Admit (and Extend, which shares its traversal) name every path from an
// implicit root: a top-level field is ".price", a nested one ".outer.b".
// Validate's ValidationError.Path predates Admit and has always been bare —
// "price", "outer.b" — because entity handler responses echo it verbatim as
// the `fieldPath` problem-detail property (see
// TestCreateEntity_IncompatibleType_ReturnsSpecificCode in
// internal/domain/entity/handler_test.go, which asserts fieldPath == "price"
// for a root-level field, not ".price"). Extend's own error strings keep
// Admit's leading dot unchanged — the change-level gate has always named a
// root-level path that way — so this trim applies only on the Validate side.
func wirePath(p string) string {
	return strings.TrimPrefix(p, ".")
}

func validationErrorFor(c Change) ValidationError {
	switch c.Reason {
	case ReasonLeafType:
		expected := make([]DataType, len(c.Declared))
		copy(expected, c.Declared)
		return ValidationError{
			Path:          wirePath(c.DocPath),
			Message:       fmt.Sprintf("value of type %s is not compatible with %v", c.Observed, c.Declared),
			Kind:          ErrKindIncompatibleType,
			ExpectedTypes: expected,
			ActualType:    c.Observed,
		}
	case ReasonNewField, ReasonInvalidFieldName:
		// Strict validation never establishes a field
		// (docs/cloud-parity/model-field-name-grammar.md), so an
		// unspellable name here is not the grammar violation Admit/Extend
		// refuse outright — it renders exactly like any other field the
		// stored model does not declare.
		return ValidationError{
			Path:    wirePath(c.DocPath),
			Message: "unexpected field not present in model",
			Kind:    ErrKindGeneric,
		}
	case ReasonNewKind:
		return validationErrorForNewKind(c)
	case ReasonArrayElement:
		// The array's element was never observed at all — there is no
		// declared element type to compare the document's content against,
		// so this does not fit the "expected X, got Y" template the other
		// reasons use (that would either misname a heterogeneous array's
		// content with a single Y, or describe the array's own JSON kind,
		// which the document DID supply correctly — the old rendering's
		// "got null" was simply wrong for a document holding a real array).
		// Name the actual situation instead: admitting content here is
		// schema learning, and c.Value (the array itself) confirms what the
		// document supplied rather than what the model failed to find.
		return ValidationError{
			Path:    wirePath(c.DocPath),
			Message: "array element type has never been observed; the document supplies " + JSONKindName(c.Value),
			Kind:    ErrKindGeneric,
		}
	default:
		return ValidationError{
			Path:    wirePath(c.DocPath),
			Message: "expected " + c.DeclaredKinds + ", got " + JSONKindName(c.Value),
			Kind:    ErrKindGeneric,
		}
	}
}

// validationErrorForNewKind renders ReasonNewKind — a value whose JSON kind
// the node does not declare at all — matching the wire wording this package
// has always used for the identical cases.
//
// A scalar value against a node with no object or array branch is exactly
// the "kind IS scalar, so the complaint is the TYPE" case, rendered
// identically to ReasonLeafType (ErrKindIncompatibleType). A container value
// against such a node gets "expected scalar, got object/array" — pinned
// verbatim by internal/e2e/model_kind_enforcement_test.go and
// internal/grpc/model_kind_enforcement_test.go. Anything else (a node that
// already declares some kind, so a genuinely new branch is being added) gets
// the generic "expected <kinds>, got <kind>".
func validationErrorForNewKind(c Change) ValidationError {
	declaresContainer := strings.Contains(c.DeclaredKinds, "object") || strings.Contains(c.DeclaredKinds, "array")

	switch c.Value.(type) {
	case json.Number, string, bool:
		if !declaresContainer {
			expected := make([]DataType, len(c.Declared))
			copy(expected, c.Declared)
			return ValidationError{
				Path:          wirePath(c.DocPath),
				Message:       fmt.Sprintf("value of type %s is not compatible with %v", c.Observed, c.Declared),
				Kind:          ErrKindIncompatibleType,
				ExpectedTypes: expected,
				ActualType:    c.Observed,
			}
		}
	case map[string]any, []any:
		if !declaresContainer {
			return ValidationError{
				Path:    wirePath(c.DocPath),
				Message: "expected scalar, got " + JSONKindName(c.Value),
				Kind:    ErrKindGeneric,
			}
		}
	}
	return ValidationError{
		Path:    wirePath(c.DocPath),
		Message: "expected " + c.DeclaredKinds + ", got " + JSONKindName(c.Value),
		Kind:    ErrKindGeneric,
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

// declaredKindNames names the kinds a container node declares, so a rejection
// tells the caller what the field does accept rather than only what it does not.
func declaredKindNames(node *ModelNode) string {
	names := make([]string, 0, len(node.Kinds()))
	if node.Object() != nil {
		names = append(names, "object")
	}
	if node.Array() != nil {
		names = append(names, "array")
	}
	if node.Scalar() != nil {
		names = append(names, "scalar")
	}
	if len(names) == 0 {
		return "no value"
	}
	return strings.Join(names, " or ")
}
