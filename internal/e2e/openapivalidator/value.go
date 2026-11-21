package openapivalidator

import (
	"errors"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

// ValidateValue checks value, a decoded JSON value, against schema, a schema
// of doc in the view of ApplyView, as the Validator checks a response body.
//
// opts are kin's options for the check. With none, they are the options the
// Validator passes for a body of doc: MultiErrors, EnableJSONSchema2020 for
// OpenAPI 3.1, and VisitAsResponse.
//
// The result is nil or an openapi3.MultiError that lists every defect once. A
// row whose JSON pointer and reason equal those of an earlier row is dropped:
// kin refuses a value no union member accepts once per member.
func ValidateValue(doc *openapi3.T, schema *openapi3.Schema, value any, opts ...openapi3.SchemaValidationOption) error {
	if len(opts) == 0 {
		opts = schemaOptions(validatorOptions(), doc)
	}
	kinErr := schema.VisitJSON(value, opts...)
	if kinErr == nil {
		return nil
	}
	var errs openapi3.MultiError
	seen := map[mismatchKey]bool{}
	for _, e := range schemaErrors(kinErr) {
		if k, ok := keyOf(e); ok {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		errs = append(errs, e)
	}
	return errs
}

// schemaOptions returns the options openapi3filter.ValidateResponse passes to
// VisitJSON for a response body (kin-openapi v0.153.0
// openapi3filter/validate_response.go:63-78 and :154). The unexported custom
// error-message option is not reproducible; the Validator does not set it.
func schemaOptions(o *openapi3filter.Options, doc *openapi3.T) []openapi3.SchemaValidationOption {
	var opts []openapi3.SchemaValidationOption
	if o.MultiError {
		opts = append(opts, openapi3.MultiErrors())
	}
	if o.ExcludeWriteOnlyValidations {
		opts = append(opts, openapi3.DisableWriteOnlyValidation())
	}
	opts = append(opts, o.SchemaValidationOptions...)
	if doc.IsOpenAPI31OrLater() {
		opts = append(opts, openapi3.EnableJSONSchema2020())
	}
	return append(opts, openapi3.VisitAsResponse())
}

// mismatchKey identifies a schema defect: the JSON pointer of the value and
// kin's reason.
type mismatchKey struct {
	pointer string
	reason  string
}

// keyOf returns the key of err, a kin error, or false when err carries no
// *openapi3.SchemaError.
func keyOf(err error) (mismatchKey, bool) {
	var se *openapi3.SchemaError
	if !errors.As(err, &se) {
		return mismatchKey{}, false
	}
	var p string
	for _, token := range se.JSONPointer() {
		p += "/" + pointerEscaper.Replace(token)
	}
	return mismatchKey{pointer: p, reason: se.Reason}, true
}

// pointerEscaper escapes a JSON pointer reference token (RFC 6901).
var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")
