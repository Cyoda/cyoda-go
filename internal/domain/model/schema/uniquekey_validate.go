package schema

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// UniqueKeyDefError is returned by ValidateUniqueKeys when one or more keys
// are invalid. Callers may use errors.As to inspect the Reason.
type UniqueKeyDefError struct {
	Reason string
}

func (e *UniqueKeyDefError) Error() string {
	return fmt.Sprintf("invalid unique key definition: %s", e.Reason)
}

// ValidateUniqueKeys verifies that every key in keys refers only to known
// scalar leaf fields in n, and that key IDs and per-key field lists are
// internally consistent (non-empty, no duplicates).
//
// A KEYED PATH MUST NOT BE POLYMORPHIC. A "scalar leaf" is therefore a path
// whose node declares no container kind — not merely one that carries a scalar
// branch alongside others. (There are only three kinds and one of them is the
// scalar, so a polymorphic node always declares a container: the test below
// covers the invariant and additionally rejects a pure container.)
//
// The reason is that a claim is computed by tokenizing the value at the path,
// and tokenizing refuses an object or an array. A key over a path that admits
// both a string and an object could be enforced for only half the values the
// path declares — so the model would declare a kind that no write can ever
// supply.
//
// This is checked on every door that can change either side of the pair: the
// sample-data import (a second import can union a new kind onto a keyed path),
// the unique-key declaration itself, and the schema extension an entity write
// performs. The last one matters most, because it commits the schema change
// BEFORE the write's own transaction opens — there would be nothing left to
// roll back.
//
// Returns *UniqueKeyDefError on first violation; nil on success.
func ValidateUniqueKeys(n *ModelNode, keys []spi.UniqueKey) error {
	// Build the set of valid scalar-leaf paths.
	scalarLeafs := make(map[string]struct{})
	for _, f := range n.Fields() {
		if f.IsArray {
			continue
		}
		if strings.ContainsAny(f.Path, "[*") {
			continue
		}
		scalarLeafs[f.Path] = struct{}{}
	}
	// Fields() reports a path once per node, so a node carrying a scalar branch
	// ALONGSIDE a container appears here exactly as a pure scalar does. Remove
	// those: the walk below is the only thing that can tell them apart.
	dropContainerPaths(n, "$", scalarLeafs)

	// Validate each key.
	seenIDs := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key.ID == "" {
			return &UniqueKeyDefError{Reason: "key ID must not be empty"}
		}
		if _, dup := seenIDs[key.ID]; dup {
			return &UniqueKeyDefError{Reason: fmt.Sprintf("duplicate key ID %q", key.ID)}
		}
		seenIDs[key.ID] = struct{}{}

		if len(key.Fields) == 0 {
			return &UniqueKeyDefError{Reason: fmt.Sprintf("key %q has no fields", key.ID)}
		}

		seenFields := make(map[string]struct{}, len(key.Fields))
		for _, field := range key.Fields {
			if _, dup := seenFields[field]; dup {
				return &UniqueKeyDefError{
					Reason: fmt.Sprintf("key %q: duplicate field %q", key.ID, field),
				}
			}
			seenFields[field] = struct{}{}

			if _, ok := scalarLeafs[field]; !ok {
				return &UniqueKeyDefError{
					Reason: fmt.Sprintf("key %q: field %q is not a known scalar leaf", key.ID, field),
				}
			}
		}
	}

	return nil
}

// dropContainerPaths removes from paths every path whose node declares a
// container kind, walking object branches with the same spelling
// [ModelNode.Fields] uses. An array branch is not descended: anything under one
// is spelled with "[*]" and has already been filtered out.
func dropContainerPaths(n *ModelNode, prefix string, paths map[string]struct{}) {
	if n.Object() != nil || n.Array() != nil {
		delete(paths, prefix)
	}
	o := n.Object()
	if o == nil {
		return
	}
	for name, child := range o.Children() {
		dropContainerPaths(child, prefix+"."+name, paths)
	}
}
