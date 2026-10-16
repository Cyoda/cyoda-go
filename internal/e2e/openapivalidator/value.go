package openapivalidator

import (
	"github.com/getkin/kin-openapi/openapi3"
)

// ValidateValue checks value, a decoded JSON value, against schema, a schema
// of doc in the view of ApplyView, as the Validator checks a response body:
// kin's VisitJSON, then the recursive walk of checkRecursive, which checks the
// values kin accepts unchecked where a schema recurs.
//
// opts are the kin options of every check. With none, they are the options
// the Validator passes for a body of doc: MultiErrors, EnableJSONSchema2020
// for OpenAPI 3.1, and VisitAsResponse.
//
// The result is nil or an openapi3.MultiError that lists every defect once:
// kin's errors, then each walk mismatch that does not repeat one of them. A
// row whose JSON pointer and reason equal those of an earlier row is dropped:
// kin refuses a value no union member accepts once per member.
func ValidateValue(doc *openapi3.T, schema *openapi3.Schema, value any, opts ...openapi3.SchemaValidationOption) error {
	if len(opts) == 0 {
		opts = schemaOptions(validatorOptions(), doc)
	}
	var errs openapi3.MultiError
	kinErr := schema.VisitJSON(value, opts...)
	if kinErr != nil {
		seen := map[mismatchKey]bool{}
		for _, e := range schemaErrors(kinErr) {
			if k, ok := keyOf("", e); ok {
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			errs = append(errs, e)
		}
	}
	errs = append(errs, recursiveErrors(schema, value, recursiveFrom(schema), opts, kinErr)...)
	if len(errs) == 0 {
		return nil
	}
	return errs
}
