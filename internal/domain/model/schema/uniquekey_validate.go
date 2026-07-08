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
// A "scalar leaf" is a FieldDescriptor from n.Fields() that is NOT marked
// IsArray and whose path does not contain "[" or "*".
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
