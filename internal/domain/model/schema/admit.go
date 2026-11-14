package schema

import (
	"encoding/json"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ChangeReason names why the stored model did not already admit a value.
type ChangeReason int

const (
	// ReasonLeafType — a leaf declares scalar types, none of which admits
	// this value.
	ReasonLeafType ChangeReason = iota
	// ReasonNewField — an object does not declare this field.
	ReasonNewField
	// ReasonNewKind — a path does not declare the value's JSON kind.
	ReasonNewKind
	// ReasonArrayElement — an array observed without content learns its
	// element type.
	ReasonArrayElement
	// ReasonNullable — a node declaring no scalar is observed as null.
	ReasonNullable
	// ReasonInvalidFieldName — a new field's name fails ValidateFieldName.
	// Only ever recorded when the admitter is built with
	// continueOnInvalidName set (Validate's own traversal): the ordinary
	// top-level Admit (and therefore Extend) still aborts the whole
	// traversal with the ValidateFieldName error itself rather than
	// recording this as a Change — establishing a field is the one thing
	// Extend must still refuse outright, unconditionally, regardless of
	// change level. Strict validation does not establish fields at all, so
	// it renders this exactly like ReasonNewField: the ordinary
	// unknown-field refusal, not one naming the grammar.
	ReasonInvalidFieldName
)

// Change is one observation the stored model does not admit, together with
// what admitting it would cost. Extend refuses the write when any change
// exceeds the configured level; Validate renders every change as a
// ValidationError.
type Change struct {
	Path string
	// DocPath is the document-instance path this change was found at:
	// ".tags[1]" for the second element of an array named "tags", carrying
	// the concrete index the document actually held — as opposed to Path,
	// the schema-op path (".tags[]") every element of an array shares,
	// because Extend's change-level gate compares models, not documents,
	// and has never had per-element indices to report. validationErrorFor
	// renders DocPath (a document error names the exact element a client
	// sent); changeLevelError keeps rendering Path (a schema-op refusal
	// names the array's element slot, not one instance of it).
	DocPath  string
	Reason   ChangeReason
	Required spi.ChangeLevel
	Observed DataType // ReasonLeafType, and ReasonNewKind for a scalar value; zero otherwise
	// Declared is the node's declared types at the moment of the change:
	// what a leaf's scalar branch declares for ReasonLeafType, or
	// model.DeclaredTypes() — [NULL] for a nullable-only node, nil for one
	// declaring nothing — for ReasonNewKind and ReasonNullable. Task 8 needs
	// it in both cases to render ErrKindIncompatibleType exactly as the old
	// Validate did.
	Declared []DataType
	// DeclaredKinds names the kinds the node declared when the change was
	// recorded — "object", "array", "scalar", joined with " or ", or
	// "no value" — in the wording Validate has always used. Recorded here so
	// error rendering never has to reach back to the node.
	DeclaredKinds string
	Value         any // the value that forced the change, for error rendering
}

// DepthExceededError marks a document that nested deeper than
// MaxValidationDepth before Admit (or Describe) could finish walking it. It
// carries the path so a caller can render a structured failure — Validate's
// ValidationError, in particular — without parsing an error string apart.
// The write path already draws this line with sentinels and typed errors
// rather than string matching (see ingest.ErrInternalSchema's doc comment,
// and handler.go's classifyValidateOrExtendErr); a bare fmt.Errorf here
// would have been the odd one out, and silently degrades the moment the
// wrapped message text drifts from whatever a caller matches against.
type DepthExceededError struct {
	Path string
}

// Error renders the same "<path>: validation depth exceeded (max N)" text
// this package has always used for the failure, so nothing downstream that
// only looks at err.Error() (e.g. admit_test.go's own assertions) needs to
// change alongside the type.
func (e *DepthExceededError) Error() string {
	return fmt.Sprintf("%s: %s", displayPath(e.Path), depthExceededMessage)
}

// UnsupportedValueError marks a value node's traversal cannot classify at
// all: a caller-side contract violation, not a client-contract one — a raw
// float64 leaking through without json.UseNumber decoding, or (per node's
// own doc comment) a Go type node never produces at all. Kind carries the
// detail for logs and Admit/Extend callers; it is deliberately NOT what
// Validate renders into a client-facing ValidationError.Message, which
// would otherwise echo a %T-formatted Go type name at the API boundary.
type UnsupportedValueError struct {
	Path string
	Kind string
}

// Error keeps the wording node has always produced for these two cases, so
// nothing that only looks at err.Error() needs to change alongside the type.
func (e *UnsupportedValueError) Error() string {
	return fmt.Sprintf("%s: %s", displayPath(e.Path), e.Kind)
}

// Admit walks data against model, one value at a time, and reports what the
// model would have to become to hold it.
//
// This is the single traversal the write path runs. The previous design
// converted the document into a throwaway model first, which discarded every
// value before any decision was made and fused array elements into one
// description — so a value-aware rule could not be evaluated at all, and
// [2147483648, "hello"] could not be judged element by element.
//
// The overlay is SPARSE: it describes only the leaves and branches that must
// change, so Merge(model, overlay) moves exactly the paths the traversal
// objected to. Merging a full walk of the document instead widened unrelated
// fields — a leaf's declared types depended on what else the document
// happened to carry.
//
// A nil overlay with no changes means the model already admits the document.
func Admit(model *ModelNode, data any) (*ModelNode, []Change, error) {
	a := &admitter{}
	overlay, err := a.node(model, data, "", "", 0, spi.ChangeLevelType)
	if err != nil {
		return nil, nil, err
	}
	return overlay, a.changes, nil
}

type admitter struct {
	changes []Change
	// continueOnInvalidName, when set, makes object()'s new-field branch
	// record an unspellable name as a Change (ReasonInvalidFieldName) and
	// keep walking the rest of the document, instead of aborting the whole
	// traversal with ValidateFieldName's error. Only Validate's own
	// traversal (validate.go) sets this — the public Admit entry point
	// (and therefore Extend) always leaves it false, so every existing
	// caller of Admit keeps seeing the abort-with-error behaviour.
	continueOnInvalidName bool
}

func (a *admitter) record(c Change) { a.changes = append(a.changes, c) }

// node admits one value against one model node.
//
// scalarLevel is the level a scalar-shaped change costs at this position:
// ChangeLevelType normally, ChangeLevelArrayElements directly on an array's
// element. It is preserved through nested array levels and reset when
// descending into an object's children, which is exactly where the element
// rules stop applying.
func (a *admitter) node(model *ModelNode, data any, path, docPath string, depth int, scalarLevel spi.ChangeLevel) (*ModelNode, error) {
	if depth >= MaxValidationDepth {
		return nil, &DepthExceededError{Path: path}
	}

	switch v := data.(type) {
	case nil:
		return a.null(model, path, docPath, scalarLevel), nil
	case map[string]any:
		if model.Object() == nil {
			return a.wrongKind(model, data, path, docPath, depth, scalarLevel)
		}
		return a.object(model, v, path, docPath, depth, scalarLevel)
	case []any:
		if model.Array() == nil {
			return a.wrongKind(model, data, path, docPath, depth, scalarLevel)
		}
		return a.array(model, v, path, docPath, depth, scalarLevel)
	case float64:
		return nil, &UnsupportedValueError{Path: path, Kind: "received float64 value; callers must use json.UseNumber() decoding"}
	case json.Number, string, bool:
		return a.scalar(model, data, path, docPath, depth, scalarLevel)
	default:
		return nil, &UnsupportedValueError{Path: path, Kind: fmt.Sprintf("unsupported type: %T", data)}
	}
}

// scalar is §4's rule: the value's JSON kind must match a declared type's
// kind, and that type must admit the value. A node with no scalar branch at
// all is the same "path gains a kind it does not declare" case a container
// value hits against a node lacking that branch — wrongKind is the single
// site of that policy.
func (a *admitter) scalar(model *ModelNode, data any, path, docPath string, depth int, scalarLevel spi.ChangeLevel) (*ModelNode, error) {
	s := model.Scalar()
	if s == nil {
		return a.wrongKind(model, data, path, docPath, depth, scalarLevel)
	}
	if holdsScalar(s.Types(), data) {
		return nil, nil
	}
	// The scalar kind IS declared here — the value's kind is not the
	// complaint, its type is.
	observed := inferDataType(data)
	a.record(Change{
		Path: path, DocPath: docPath, Reason: ReasonLeafType, Required: scalarLevel,
		Observed: observed, Declared: s.Types(), DeclaredKinds: declaredKindNames(model), Value: data,
	})
	return NewLeafNode(observed), nil
}

// holdsScalar answers §4's per-kind table for one value against one declared
// set. A JSON number is not a string and a JSON string is not a number, so
// STRING does not become a universal sink.
func holdsScalar(declared []DataType, data any) bool {
	switch v := data.(type) {
	case json.Number:
		dec, err := ParseDecimal(string(v))
		if err != nil {
			return false
		}
		for _, dt := range declared {
			if AdmitsNumeric(dt, dec) {
				return true
			}
		}
		return false

	case string:
		// Every JSON string is a string. A temporal type additionally admits
		// a string whose classification is EXACTLY that type: a
		// LOCAL_DATE_TIME parser accepts an offset-bearing timestamp by
		// discarding the offset, and those denote different instants, so the
		// value would be stored unfindable.
		classified, isTemporal := ClassifyTemporalString(v)
		for _, dt := range declared {
			if dt == String {
				return true
			}
			if isTemporal && dt == classified {
				return true
			}
		}
		return false

	case bool:
		for _, dt := range declared {
			if dt == Boolean {
				return true
			}
		}
		return false
	}
	return false
}

// null is the nullable marker. A node that already declares a scalar admits
// null and records nothing; a node that declares none charges the promotion.
func (a *admitter) null(model *ModelNode, path, docPath string, scalarLevel spi.ChangeLevel) *ModelNode {
	if model.Nullable() || model.Scalar() != nil {
		return nil
	}
	a.record(Change{
		Path: path, DocPath: docPath, Reason: ReasonNullable, Required: scalarLevel,
		Observed: Null, Declared: model.DeclaredTypes(), DeclaredKinds: declaredKindNames(model),
	})
	overlay := NewLeafNode(Null)
	overlay.SetNullable()
	return overlay
}

// wrongKind is the single site of "a path gains a kind it does not declare":
// a container against a node lacking that branch (called directly from
// node), and a scalar against a node with no scalar branch (called from
// scalar). Establishing the first kind on a node that declares none is the
// nullable-marker promotion, which keeps the level it has always had; adding
// a kind beside one already declared is a new branch, more fundamental than
// a new field.
//
// For a container value the overlay is derived with describeAt rather than
// returned as an empty container: an empty object or an array whose element
// is Null would merge to an overlay that admits nothing the document actually
// carried. describeAt continues at the SAME depth wrongKind itself received —
// wrongKind stands in for object/array at this exact position when the
// branch does not exist, and object/array are always called with the depth
// the enclosing node call received, not depth+1 — so the MaxValidationDepth
// guard node already ran for this position applies unchanged. describeAt can
// still fail one or more levels down: a deeply nested container can exceed
// MaxValidationDepth, or an object inside it can carry a field name the query
// grammar cannot address — so wrongKind reports that failure rather than
// swallowing it.
func (a *admitter) wrongKind(model *ModelNode, data any, path, docPath string, depth int, scalarLevel spi.ChangeLevel) (*ModelNode, error) {
	required := spi.ChangeLevelStructural
	if len(model.Kinds()) == 0 {
		required = scalarLevel
	}
	var observed DataType
	switch data.(type) {
	case json.Number, string, bool:
		observed = inferDataType(data)
	}
	a.record(Change{
		Path: path, DocPath: docPath, Reason: ReasonNewKind, Required: required,
		Observed: observed, Declared: model.DeclaredTypes(), DeclaredKinds: declaredKindNames(model), Value: data,
	})
	switch data.(type) {
	case map[string]any, []any:
		return a.describeAt(data, path, docPath, depth)
	default:
		return NewLeafNode(observed), nil
	}
}

// object admits a JSON object against a node's object branch. Children of an
// object are ordinary positions again: an array's element rules do not reach
// through a nested object.
//
// scalarLevel is deliberately not read here: every child is re-entered at the
// fixed spi.ChangeLevelType below, which IS the reset the scalarLevel
// discipline requires on descent into an object — the element-cost levels an
// array's contents carry never reach through a nested object. Do not thread
// the parameter through in its place — that would restore whatever level was
// in force above the object, defeating the reset.
func (a *admitter) object(model *ModelNode, m map[string]any, path, docPath string, depth int, scalarLevel spi.ChangeLevel) (*ModelNode, error) {
	var overlay *ModelNode
	ensure := func() *ModelNode {
		if overlay == nil {
			overlay = NewObjectNode()
		}
		return overlay
	}

	obj := model.Object()
	for name, val := range m {
		childPath := path + "." + name
		childDocPath := docPath + "." + name
		child := obj.Child(name)

		if child == nil {
			// A field the model does not declare. Validate the key before it
			// can become a schema field — this is the one point both
			// field-set-establishing ingresses share.
			if err := ValidateFieldName(path, name); err != nil {
				if !a.continueOnInvalidName {
					return nil, err
				}
				// Validate's traversal: this path never establishes a
				// field (docs/cloud-parity/model-field-name-grammar.md),
				// so an unspellable name is not a grammar violation here —
				// it is simply a field the stored model does not declare,
				// the ordinary unknown-field signal. Record it as such and
				// keep walking the rest of the document: unlike Extend,
				// strict validation must not abort at the first offender.
				a.record(Change{
					Path: childPath, DocPath: childDocPath, Reason: ReasonInvalidFieldName,
					Required: spi.ChangeLevelStructural, DeclaredKinds: declaredKindNames(model), Value: val,
				})
				continue
			}
			a.record(Change{
				Path: childPath, DocPath: childDocPath, Reason: ReasonNewField,
				Required: spi.ChangeLevelStructural, DeclaredKinds: declaredKindNames(model), Value: val,
			})
			derived, err := a.describeAt(val, childPath, childDocPath, depth+1)
			if err != nil {
				return nil, err
			}
			ensure().SetChild(name, derived)
			continue
		}

		childOverlay, err := a.node(child, val, childPath, childDocPath, depth+1, spi.ChangeLevelType)
		if err != nil {
			return nil, err
		}
		if childOverlay != nil {
			ensure().SetChild(name, childOverlay)
		}
	}
	// A field the model declares but the document omits is not a change: the
	// model describes known structure, not required fields.
	return overlay, nil
}

// array admits a JSON array against a node's array branch, judging each
// element individually. The old walk fused every element into one description
// before anything was judged, so [2147483648, "hello"] became a single entry
// meaning "large integer or text" and the number had already been widened.
//
// scalarLevel is deliberately not read here: an already-typed element always
// recurses at the fixed spi.ChangeLevelArrayElements below, and a
// newly-learned element is charged that same level directly — both are a
// re-assertion of the level an array's own contents always cost, not a
// value threaded down from above. Do not thread the parameter through in
// its place.
func (a *admitter) array(model *ModelNode, arr []any, path, docPath string, depth int, scalarLevel spi.ChangeLevel) (*ModelNode, error) {
	exArr := model.Array()
	elemPath := path + "[]"

	var elemOverlay *ModelNode

	if exArr.Element() == nil {
		// The array was observed, but never with content, so it declares no
		// element type. Learning one is the same promotion a node declaring
		// no kind undergoes, at the level an array element's changes cost —
		// and an EMPTY array still triggers it: Describe has always
		// represented [] as an array whose element is Null (the
		// NewLeafNode(Null) below), so a derived array always has
		// a non-nil element, empty or not — charging ARRAY_ELEMENTS whenever
		// the existing element is nil and the document supplies an array at
		// all, with no separate case for an empty one, means charging it
		// regardless of len(arr).
		//
		// Path is the array's own path, not elemPath, and Value is the
		// document's array itself: this Change is about the array's element
		// never having been observed at all, not about any one element's
		// content, so both renderers (changeLevelError, validationErrorFor)
		// name the array and describe that situation directly rather than
		// reaching for a value they'd have to pick one element out of.
		a.record(Change{
			Path: path, DocPath: docPath, Reason: ReasonArrayElement,
			Required: spi.ChangeLevelArrayElements, DeclaredKinds: declaredKindNames(model), Value: arr,
		})
		if len(arr) == 0 {
			elemOverlay = NewLeafNode(Null)
		} else {
			for i, item := range arr {
				itemDocPath := fmt.Sprintf("%s[%d]", docPath, i)
				derived, err := a.describeAt(item, elemPath, itemDocPath, depth+1)
				if err != nil {
					return nil, err
				}
				if elemOverlay == nil {
					elemOverlay = derived
				} else {
					elemOverlay = Merge(elemOverlay, derived)
				}
			}
		}
	} else {
		// ARRAY_ELEMENTS applies at an array's element and keeps applying
		// through further array levels.
		for i, item := range arr {
			itemDocPath := fmt.Sprintf("%s[%d]", docPath, i)
			itemOverlay, err := a.node(exArr.Element(), item, elemPath, itemDocPath, depth+1, spi.ChangeLevelArrayElements)
			if err != nil {
				return nil, err
			}
			if itemOverlay == nil {
				continue
			}
			if elemOverlay == nil {
				elemOverlay = itemOverlay
			} else {
				elemOverlay = Merge(elemOverlay, itemOverlay)
			}
		}
	}

	// An array's length is not part of the model: the branch declares its
	// element, and a list of any length is held by that element. Only the
	// element can change.
	if elemOverlay == nil {
		return nil, nil
	}
	return NewArrayNode(elemOverlay), nil
}

// Describe derives the model fragment a value implies, with no stored model
// to compare against — the shape a brand-new field or element takes. It is
// the depth-0 entry point into describeAt; see that doc comment for the
// mechanics.
func Describe(v any, path string) (*ModelNode, error) {
	a := &admitter{}
	return a.describeAt(v, path, path, 0)
}

// describeAt is Describe's recursive form, carrying the depth a brand-new
// subtree has reached so it stays bounded by the same MaxValidationDepth
// guard node enforces. Every value under a brand-new field or element is
// itself new, so object's new-field branch and array's element-learning
// branch both re-enter describeAt rather than node — and without threading
// depth through those re-entries, a document nested deeper than
// MaxValidationDepth under a single brand-new field would recurse
// unbounded instead of failing closed: node's own guard is never reached,
// because none of these calls go through node.
//
// It is a method on the CALLING admitter — not a free function building its
// own from scratch — specifically so a.continueOnInvalidName reaches the
// inner traversal: re-review finding #2 was that a bare &admitter{} here
// silently reverted to the abort-on-first-bad-name behaviour for anything
// nested inside a brand-new subtree, which strict validation had already
// turned off at the top level. It still runs that inner traversal in its
// own throwaway admitter (`inner`), not `a` itself, and still discards
// `inner.changes` wholesale EXCEPT for ReasonInvalidFieldName entries: the
// caller already recorded its own single Change for whatever triggered this
// describeAt call (a new field, a wrong-kind container, a learned array
// element), so echoing every ordinary field/type/kind change from inside a
// brand-new subtree would be redundant — but an unspellable nested name is
// not redundant, it is the ONE thing this subtree can still fail on under
// continueOnInvalidName, and it must reach Validate's real changes list or
// it is silently dropped with no diagnostic at all. Multi-level nesting
// (a new field inside a new field inside a new field) still surfaces: each
// describeAt call escalates from its own `inner` to its own `a`, chaining
// all the way up to the outermost admitter Validate built.
//
// A container value dispatches straight into object/array against a node
// that already declares the container's kind but has no children, rather
// than through node against emptyNode(): emptyNode declares no branch at
// all, so node would route a container back through wrongKind, which for a
// container value calls describeAt — looping forever.
//
// object's empty-object case ({}) still needs an explicit non-nil fallback:
// object returns nil for "no change" when its input map has no keys, which
// is indistinguishable at that call site from "nothing new here" — but
// a derived model has always recorded an empty object as declaring
// KindObject with no children (a bare NewObjectNode()), and describeAt must
// produce the same shape or a field whose only observed value is {} would
// vanish from the derived model instead of declaring an (empty) branch.
// array needs no equivalent fallback: it is seeded with a nil element here,
// so it always takes the "array declares no element yet" branch below, which
// unconditionally charges and sets an element — including NewLeafNode(Null)
// for an empty array, so [] derives as NewArrayNode(NewLeafNode(Null)) — so
// array never returns nil when called from here.
func (a *admitter) describeAt(v any, path, docPath string, depth int) (*ModelNode, error) {
	if depth >= MaxValidationDepth {
		return nil, &DepthExceededError{Path: path}
	}
	inner := &admitter{continueOnInvalidName: a.continueOnInvalidName}
	var overlay *ModelNode
	var err error
	switch val := v.(type) {
	case map[string]any:
		overlay, err = inner.object(NewObjectNode(), val, path, docPath, depth, spi.ChangeLevelType)
		if overlay == nil && err == nil {
			overlay = NewObjectNode()
		}
	case []any:
		overlay, err = inner.array(NewArrayNode(nil), val, path, docPath, depth, spi.ChangeLevelType)
	default:
		overlay, err = inner.node(emptyNode(), v, path, docPath, depth, spi.ChangeLevelType)
	}
	for _, c := range inner.changes {
		if c.Reason == ReasonInvalidFieldName {
			a.record(c)
		}
	}
	if err != nil {
		return nil, err
	}
	return overlay, nil
}

// emptyNode is a node declaring nothing: every value is a change against it.
func emptyNode() *ModelNode { return spi.NewEmptyNode() }
