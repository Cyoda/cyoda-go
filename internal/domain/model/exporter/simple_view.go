package exporter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// SimpleViewExporter exports a ModelNode tree as Cyoda's native SIMPLE_VIEW
// format: a JSON object with "currentState" and a path-based "model" map.
type SimpleViewExporter struct {
	state string
}

// NewSimpleViewExporter returns a SimpleViewExporter with the given lock state
// (typically "LOCKED" or "UNLOCKED").
func NewSimpleViewExporter(currentState string) *SimpleViewExporter {
	return &SimpleViewExporter{state: currentState}
}

// Export converts the ModelNode tree into a SIMPLE_VIEW JSON byte slice.
func (e *SimpleViewExporter) Export(node *schema.ModelNode) ([]byte, error) {
	model := make(map[string]map[string]any)
	e.walk(node, "$", model)

	result := map[string]any{
		"currentState": e.state,
		"model":        sortedModel(model),
	}
	return json.Marshal(result)
}

// walk builds the descriptor bucket for one object node at path, recursing
// into the substructure that needs buckets of its own.
func (e *SimpleViewExporter) walk(node *schema.ModelNode, path string, model map[string]map[string]any) {
	if node.Object() == nil {
		return
	}

	descriptor := make(map[string]any)
	children := node.Object().Children()

	// Sort child keys for deterministic output.
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		e.describeChild(children[name], name, path, descriptor, model)
	}

	model[path] = descriptor
}

// describeChild writes the entries describing one named child into its parent's
// bucket.
//
// A node is described by the branches it actually carries, not by its dominant
// Kind: a field observed as both a scalar and a container declares — and
// enforces — both kinds, and Merge records that as scalar types sitting on a
// structural node. Rendering only the structural branch made two models that
// enforce differently render identically.
func (e *SimpleViewExporter) describeChild(
	child *schema.ModelNode, name, parentPath string,
	desc map[string]any, model map[string]map[string]any,
) {
	if child.Object() == nil && child.Array() == nil {
		desc["."+name] = typeNames(child.DeclaredTypes())
		return
	}

	// Scalar branch of a kind union. NULL alone is the nullable marker, not a
	// scalar observation, so it does not open one.
	if sc := child.Scalar(); sc != nil {
		desc["."+name] = typeNames(sc.Types())
	}

	if child.Object() != nil {
		desc["#."+name] = "OBJECT"
		e.walk(child, parentPath+"."+name, model)
	}

	// Array branch. Present independently of Kind: Merge promotes an
	// object-and-array union to KindObject while keeping the element.
	switch {
	case child.Array() != nil && child.Array().Element() != nil:
		e.describeElements(child, name, parentPath, "[*]", desc, model)
	case child.Array() != nil:
		// An array whose elements were never observed — the empty-array seed
		// the codec preserves. The level is declared and enforced, so it is
		// named; its element type is not known.
		desc["."+name+"[*]"] = "NULL"
	}
}

// describeElements describes the elements of arr — a node carrying an array
// branch — under the accumulated wildcard suffix. One "[*]" per array level,
// so an array of arrays is addressed the way the field paths and the search
// surface address it, and the elements are themselves described by the
// branches they carry.
func (e *SimpleViewExporter) describeElements(
	arr *schema.ModelNode, name, parentPath, suffix string,
	desc map[string]any, model map[string]map[string]any,
) {
	elem := arr.Array().Element()
	if elem.Object() == nil && elem.Array() == nil {
		// The width is this array's: it is the one these elements belong to.
		desc["."+name+suffix] = widthDescriptor(typeNames(elem.DeclaredTypes()), arr)
		return
	}

	// Scalar branch of elements observed as both a scalar and a container.
	if sc := elem.Scalar(); sc != nil {
		desc["."+name+suffix] = widthDescriptor(typeNames(sc.Types()), arr)
	}

	if elem.Object() != nil {
		// The elements carry a structure, so they get a bucket of their own.
		desc["#."+name] = "OBJECT"
		elemPath := parentPath + "." + name + suffix
		e.walk(elem, elemPath, model)
		if bucket, ok := model[elemPath]; ok {
			bucket["#"] = "ARRAY_ELEMENT"
		}
	}

	if elem.Array() != nil && elem.Array().Element() != nil {
		e.describeElements(elem, name, parentPath, suffix+"[*]", desc, model)
		return
	}
	if elem.Array() != nil {
		// An array level whose own elements were never observed: the level is
		// declared, its element type is not.
		desc["."+name+suffix+"[*]"] = "NULL"
	}
}

// typeNames formats DataTypes as a SIMPLE_VIEW type descriptor string.
func typeNames(types []schema.DataType) string {
	if len(types) == 0 {
		return "NULL"
	}
	if len(types) == 1 {
		return types[0].String()
	}
	// Polymorphic: "[TYPE1, TYPE2, ...]"
	names := make([]string, len(types))
	for i, dt := range types {
		names[i] = dt.String()
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// widthDescriptor decorates an element type descriptor with the widest array
// observed at that level, when one was recorded.
func widthDescriptor(td string, arr *schema.ModelNode) string {
	if a := arr.Array(); a != nil && a.MaxWidth() > 0 {
		return fmt.Sprintf("(%s x %d)", td, a.MaxWidth())
	}
	return td
}

// sortedModel returns an ordered map representation for deterministic JSON output.
func sortedModel(model map[string]map[string]any) json.Marshaler {
	return &orderedModel{data: model}
}

type orderedModel struct {
	data map[string]map[string]any
}

func (m *orderedModel) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf strings.Builder
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		valJSON, err := json.Marshal(m.data[k])
		if err != nil {
			return nil, err
		}
		buf.Write(valJSON)
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}
