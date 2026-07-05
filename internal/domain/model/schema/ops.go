package schema

import (
	"encoding/json"
	"fmt"
	"slices"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// SchemaOpKind enumerates the catalog of additive schema operations.
// The catalog is the minimal set covering every change class that
// schema.Extend emits at non-zero ChangeLevel (ARRAY_ELEMENTS, TYPE,
// STRUCTURAL). Every kind satisfies both commutativity and
// validation-monotonicity (property tests in properties_test.go).
//
// Wire-format note: the kind strings are persisted in plugin extension
// logs and gossiped between cyoda-go versions. Adding a kind is a
// forward-incompatible change across versions — handle in
// coordination with the plugin migration story.
type SchemaOpKind string

const (
	// KindAddProperty inserts or merges a child sub-tree into an
	// OBJECT node at the operation's Path. The Name field carries the
	// new child key; the Payload field carries the encoded *ModelNode
	// sub-tree (same encoding as schema.Marshal). ChangeLevel:
	// STRUCTURAL.
	KindAddProperty SchemaOpKind = "add_property"

	// KindBroadenType widens a node's TypeSet by unioning added primitive
	// types. Payload is a JSON array of DataType.String() names
	// (e.g. ["NULL","STRING"]). For LEAF nodes this broadens the allowed
	// primitive types; for OBJECT and ARRAY nodes the only meaningful
	// addition is NULL (nullable marker). ChangeLevel: TYPE.
	KindBroadenType SchemaOpKind = "broaden_type"

	// KindAddArrayItemType widens an ARRAY node's element LEAF with
	// additional primitive data types. Payload shape matches
	// KindBroadenType. The op's Path targets the ARRAY node; the
	// widening applies to its .Element().Types(). An ARRAY whose
	// Element() is nil at the time of Apply gains a fresh LEAF element
	// with the payload types. ChangeLevel: ARRAY_ELEMENTS.
	KindAddArrayItemType SchemaOpKind = "add_array_item_type"
)

// SchemaOp is one entry in a serialized SchemaDelta.
//
// Path convention: slash-separated segments rooted at the model's
// root node. Empty string targets the root. Segments are either:
//   - a literal child name (for object descent), or
//   - the token "[]" (for array-element descent — valid only when
//     the current node is an ARRAY; Apply follows Element()).
//
// No JSON-Schema keywords (no "/properties", "/type"): paths are
// domain-tree field names only. Examples:
//   - "address/zip"           — zip leaf inside the address object
//   - "items/[]/field"        — field inside each element of the items array
//   - "grid/[]/[]"            — inner leaf of an array-of-arrays
//
// For KindAddArrayItemType, Path targets the ARRAY node itself; the
// widening is implicitly on its element leaf.
type SchemaOp struct {
	Kind    SchemaOpKind    `json:"kind"`
	Path    string          `json:"path"`
	Name    string          `json:"name,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewAddProperty builds an op that adds or merges child `name` into
// the OBJECT node at `parentPath`. `subtree` must be a non-nil
// encoded ModelNode produced by schema.Marshal.
func NewAddProperty(parentPath, name string, subtree []byte) SchemaOp {
	return SchemaOp{
		Kind:    KindAddProperty,
		Path:    parentPath,
		Name:    name,
		Payload: append(json.RawMessage(nil), subtree...),
	}
}

// NewBroadenType builds an op that unions the given primitive data
// types into the node at `targetPath`. For LEAF targets this broadens
// the allowed primitive types; for OBJECT and ARRAY targets only NULL
// is meaningful (adds a nullable marker).
func NewBroadenType(targetPath string, added []DataType) (SchemaOp, error) {
	payload, err := encodeTypeNames(added)
	if err != nil {
		return SchemaOp{}, fmt.Errorf("NewBroadenType: %w", err)
	}
	return SchemaOp{Kind: KindBroadenType, Path: targetPath, Payload: payload}, nil
}

// NewAddArrayItemType builds an op that unions the given primitive
// data types into the element LEAF of the ARRAY node at `arrayPath`.
func NewAddArrayItemType(arrayPath string, added []DataType) (SchemaOp, error) {
	payload, err := encodeTypeNames(added)
	if err != nil {
		return SchemaOp{}, fmt.Errorf("NewAddArrayItemType: %w", err)
	}
	return SchemaOp{Kind: KindAddArrayItemType, Path: arrayPath, Payload: payload}, nil
}

// encodeTypeNames serializes a set of DataType values to a stable,
// order-independent JSON array of canonical names.
func encodeTypeNames(types []DataType) (json.RawMessage, error) {
	if len(types) == 0 {
		return nil, fmt.Errorf("at least one DataType required")
	}
	names := make([]string, 0, len(types))
	seen := make(map[string]struct{}, len(types))
	for _, dt := range types {
		n := dt.String()
		if n == "UNKNOWN" {
			return nil, fmt.Errorf("unknown DataType: %d", int(dt))
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	slices.Sort(names)
	return json.Marshal(names)
}

// DecodeTypeNames is the inverse of encodeTypeNames.
func DecodeTypeNames(payload json.RawMessage) ([]DataType, error) {
	var names []string
	if err := json.Unmarshal(payload, &names); err != nil {
		return nil, fmt.Errorf("decode type names: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("empty type list")
	}
	out := make([]DataType, 0, len(names))
	for _, n := range names {
		dt, ok := ParseDataType(n)
		if !ok {
			return nil, fmt.Errorf("unknown DataType name %q", n)
		}
		out = append(out, dt)
	}
	return out, nil
}

// MarshalDelta serializes an op list into the opaque bytes that the
// SPI carries on ExtendSchema.
func MarshalDelta(ops []SchemaOp) (spi.SchemaDelta, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("MarshalDelta: %w", err)
	}
	return spi.SchemaDelta(b), nil
}

// UnmarshalDelta is the inverse of MarshalDelta.
func UnmarshalDelta(delta spi.SchemaDelta) ([]SchemaOp, error) {
	if len(delta) == 0 {
		return nil, nil
	}
	var ops []SchemaOp
	if err := json.Unmarshal(delta, &ops); err != nil {
		return nil, fmt.Errorf("UnmarshalDelta: %w", err)
	}
	return ops, nil
}
