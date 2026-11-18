package importer

import (
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

// rootPath is the canonical path of the document root — the same "$" prefix
// ModelNode.FieldsMap keys its entries by, so a walk-time diagnostic names a
// location in the vocabulary the rest of the stack uses.
const rootPath = "$"

// Walk converts a generic parsed data tree into a ModelNode schema tree.
//
// It delegates to schema.Describe, the recursive derivation Admit already
// runs — against a node that admits nothing — to describe a brand-new field
// or element (see admit.go's describeAt). A sample document walked at
// registration and a value admitted at write time are the same shape,
// described by the same traversal; Walk supplies only what is specific to
// this ingress, the "$" root path.
//
// Field-name validation (schema.ValidateFieldName, wrapping
// schema.ErrInvalidFieldName) and the float64 rejection are both enforced
// inside Describe's traversal, so Walk does not repeat them.
func Walk(data any) (*schema.ModelNode, error) {
	return schema.Describe(data, rootPath)
}
