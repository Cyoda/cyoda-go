package schema

import (
	"encoding/json"
	"fmt"
)

// wireNode is the JSON-serializable representation of a ModelNode.
type wireNode struct {
	Kind     string               `json:"kind"`
	Types    []string             `json:"types,omitempty"`
	Children map[string]*wireNode `json:"children,omitempty"`
	Element  *wireNode            `json:"element,omitempty"`
}

// toWire converts a ModelNode tree into a wireNode tree.
func toWire(n *ModelNode) *wireNode {
	w := &wireNode{
		Kind: n.kind.String(),
	}

	// Serialize types.
	for _, dt := range n.types.Types() {
		w.Types = append(w.Types, dt.String())
	}

	// Serialize children (object nodes).
	if n.children != nil && len(n.children) > 0 {
		w.Children = make(map[string]*wireNode, len(n.children))
		for name, child := range n.children {
			w.Children[name] = toWire(child)
		}
	}

	// Serialize element descriptor (array nodes).
	if n.element != nil {
		w.Element = toWire(n.element)
	}

	return w
}

// fromWire converts a wireNode tree back into a ModelNode tree.
//
// Every branch the wire form carries is restored, independently of the node's
// Kind: Merge records a field observed as both an object and an array as a
// KindObject node that KEPT its element, and toWire writes that element out.
// Restoring only the children would drop the array branch on the first read
// back — silently narrowing a stored model until it refused a document it was
// derived from.
func fromWire(w *wireNode) (*ModelNode, error) {
	var n *ModelNode

	switch w.Kind {
	case "OBJECT":
		n = NewObjectNode()

	case "ARRAY":
		// When the wire form has no element, preserve that: an
		// unobserved-element ARRAY round-trips to an ARRAY with
		// Element()==nil. Diff/Apply handle this as the empty-array
		// seed case (see diffArray/applyAddArrayItemType).
		n = NewArrayNode(nil)

	case "LEAF":
		n = &ModelNode{
			kind:  KindLeaf,
			types: NewTypeSet(),
		}

	default:
		return nil, fmt.Errorf("unknown node kind %q", w.Kind)
	}

	for name, wChild := range w.Children {
		child, err := fromWire(wChild)
		if err != nil {
			return nil, fmt.Errorf("child %q: %w", name, err)
		}
		n.SetChild(name, child)
	}

	if w.Element != nil {
		elem, err := fromWire(w.Element)
		if err != nil {
			return nil, fmt.Errorf("array element: %w", err)
		}
		n.element = elem
		if n.info == nil {
			// A node that carries an array branch carries its ArrayInfo too,
			// so a union restored from the wire has the same shape the
			// in-memory Merge produces.
			n.info = NewArrayInfo()
		}
	}

	// Restore types.
	for _, name := range w.Types {
		dt, ok := ParseDataType(name)
		if !ok {
			return nil, fmt.Errorf("unknown data type %q", name)
		}
		n.types.Add(dt)
	}

	return n, nil
}

// Marshal serializes a ModelNode tree to JSON bytes.
func Marshal(n *ModelNode) ([]byte, error) {
	return json.Marshal(toWire(n))
}

// Unmarshal deserializes JSON bytes into a ModelNode tree.
func Unmarshal(data []byte) (*ModelNode, error) {
	var w wireNode
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("failed to unmarshal schema: %w", err)
	}
	return fromWire(&w)
}
