package importer

import (
	"errors"
	"fmt"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ErrNonDocumentSampleData is returned when a sample-data body is neither a
// JSON object nor an array of JSON objects. Both are readings of "here is what
// my entities look like"; a scalar at the root, or a non-object inside the
// array, is not — and the model derived from one would describe an entity
// shape the entity ingress cannot accept.
//
// The service layer maps it to 400 VALIDATION_FAILED: the body parsed, so this
// is a content-level contract violation with a concrete remedy, not a parse
// failure.
var ErrNonDocumentSampleData = errors.New("sample data must be a JSON object, or an array of JSON objects")

// walkSampleData derives a model from a parsed sample-data body.
//
// A JSON array at the root is a collection of sample documents, the same
// reading the entity ingress gives an array body ("a collection of entities of
// the same type"), and the derived model is their merge — identical to what
// successive imports onto an UNLOCKED model produce. Reading it as "the entity
// is an array" instead would register a model that renders as empty and
// refuses the very documents it was derived from.
func walkSampleData(parsed any) (*schema.ModelNode, error) {
	switch v := parsed.(type) {
	case map[string]any:
		return Walk(v)
	case []any:
		node := schema.NewObjectNode()
		for i, item := range v {
			doc, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: element %d is %s", ErrNonDocumentSampleData, i, kindPhrase(item))
			}
			derived, err := Walk(doc)
			if err != nil {
				return nil, err
			}
			node = schema.Merge(node, derived)
		}
		return node, nil
	default:
		return nil, fmt.Errorf("%w: got %s", ErrNonDocumentSampleData, kindPhrase(parsed))
	}
}

// kindPhrase names a value's JSON kind as it reads inside a sentence.
func kindPhrase(v any) string {
	kind := schema.JSONKindName(v)
	switch kind {
	case "null":
		return kind
	case "object", "array":
		return "an " + kind
	default:
		return "a " + kind
	}
}
