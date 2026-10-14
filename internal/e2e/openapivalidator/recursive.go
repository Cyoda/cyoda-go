package openapivalidator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

// checkRecursive validates the parts of a JSON response body that kin-openapi
// skips inside recursive schemas.
//
// kin-openapi v0.149.0 (*Schema).visitJSON (openapi3/schema.go:2011-2019)
// keeps the set of schemas it is visiting, keyed only by *Schema pointer, and
// returns nil (valid) when it enters a schema that is already in the set,
// whatever the value. A value reached through a recursive schema — a group
// condition inside a group condition — is therefore never checked.
//
// checkRecursive walks the decoded body alongside the response schema and
// keeps the same stack of schemas that kin keeps. Where the walk enters a
// schema that is already on the stack — where kin stops — it validates that
// sub-value with a fresh VisitJSON, and so a new visited set. It then
// continues into the sub-value with a stack that holds only that schema,
// which is the stack of the fresh visit. Each mismatch is wrapped with the
// JSON pointer of the sub-value. The result is nil or an openapi3.MultiError.
func checkRecursive(route *routers.Route, opts *openapi3filter.Options, req *http.Request, status int, header http.Header, body []byte) error {
	schema := responseSchema(route, req, status, header)
	if schema == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // as openapi3filter.JSONBodyDecoder does
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil // kin reports the undecodable body
	}
	w := &recursiveWalk{
		opts:    schemaOptions(opts, route.Spec),
		checked: map[reentry]bool{},
	}
	w.walk(schema, value, "", map[*openapi3.Schema]bool{})
	if len(w.errs) == 0 {
		return nil
	}
	return w.errs
}

// responseSchema returns the JSON body schema that openapi3filter.ValidateResponse
// validates for this response, or nil when it validates no JSON body
// (kin-openapi v0.149.0 openapi3filter/validate_response.go:22-120).
func responseSchema(route *routers.Route, req *http.Request, status int, header http.Header) *openapi3.Schema {
	if req.Method == http.MethodHead {
		return nil
	}
	switch status {
	case http.StatusNotModified, http.StatusPermanentRedirect,
		http.StatusTemporaryRedirect, http.StatusMovedPermanently:
		return nil
	}
	if route.Operation == nil || route.Operation.Responses == nil {
		return nil
	}
	ref := route.Operation.Responses.Status(status)
	if ref == nil {
		ref = route.Operation.Responses.Default()
	}
	if ref == nil || ref.Value == nil {
		return nil
	}
	ct := header.Get("Content-Type")
	media, _, err := mime.ParseMediaType(ct)
	if err != nil || (media != "application/json" && !strings.HasSuffix(media, "+json")) {
		return nil
	}
	content := ref.Value.Content.Get(ct)
	if content == nil || content.Schema == nil {
		return nil
	}
	return content.Schema.Value
}

// schemaOptions returns the options openapi3filter.ValidateResponse passes to
// VisitJSON for a response body (kin-openapi v0.149.0
// openapi3filter/validate_response.go:63-78 and :147). The unexported custom
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

// reentry identifies one fresh validation: a schema at a JSON pointer.
type reentry struct {
	schema  *openapi3.Schema
	pointer string
}

type recursiveWalk struct {
	opts    []openapi3.SchemaValidationOption
	checked map[reentry]bool // makes each re-entry validate once, and the walk finite
	errs    openapi3.MultiError
}

// walk visits value with schema s, as kin's visitJSON does, with stack the
// set of schemas kin's visit has entered at this point.
func (w *recursiveWalk) walk(s *openapi3.Schema, value any, pointer string, stack map[*openapi3.Schema]bool) {
	if s == nil {
		return
	}
	if stack[s] {
		key := reentry{s, pointer}
		if w.checked[key] {
			return
		}
		w.checked[key] = true
		if err := s.VisitJSON(value, w.opts...); err != nil {
			for _, e := range schemaErrors(err) {
				w.errs = append(w.errs, fmt.Errorf("response body at %q: %w", pointer, e))
			}
		}
		stack = map[*openapi3.Schema]bool{}
	}
	stack[s] = true
	defer delete(stack, s)

	for _, ref := range s.AllOf {
		w.walk(ref.Value, value, pointer, stack)
	}
	for _, ref := range w.candidates(s, s.OneOf, value) {
		w.walk(ref.Value, value, pointer, stack)
	}
	for _, ref := range w.candidates(s, s.AnyOf, value) {
		w.walk(ref.Value, value, pointer, stack)
	}

	switch v := value.(type) {
	case map[string]any:
		if !s.Type.Permits(openapi3.TypeObject) {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			child := pointer + "/" + pointerEscaper.Replace(k)
			if p, ok := s.Properties[k]; ok {
				w.walk(p.Value, v[k], child, stack)
			} else if ap := s.AdditionalProperties.Schema; ap != nil {
				w.walk(ap.Value, v[k], child, stack)
			}
		}
	case []any:
		if !s.Type.Permits(openapi3.TypeArray) {
			return
		}
		for i, item := range v {
			ref := s.Items
			if i < len(s.PrefixItems) {
				ref = s.PrefixItems[i]
			}
			if ref != nil {
				w.walk(ref.Value, item, pointer+"/"+strconv.Itoa(i), stack)
			}
		}
	}
}

// candidates returns the oneOf/anyOf members the walk follows: with a
// discriminator mapping that names the value's discriminator, only the mapped
// member, as kin does (openapi3/schema.go:2234-2272, :2298); otherwise every
// member. Of more than one candidate, only those the value validates against
// are followed.
func (w *recursiveWalk) candidates(s *openapi3.Schema, members openapi3.SchemaRefs, value any) openapi3.SchemaRefs {
	if mapped := mappedRef(s, value); mapped != "" {
		members = slices.DeleteFunc(slices.Clone(members), func(m *openapi3.SchemaRef) bool { return m.Ref != mapped })
	}
	if len(members) <= 1 {
		return members
	}
	return slices.DeleteFunc(slices.Clone(members), func(m *openapi3.SchemaRef) bool {
		return m.Value == nil || m.Value.VisitJSON(value, w.opts...) != nil
	})
}

// mappedRef returns the $ref the discriminator mapping names for value, or "".
func mappedRef(s *openapi3.Schema, value any) string {
	if s.Discriminator == nil {
		return ""
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	name, ok := obj[s.Discriminator.PropertyName].(string)
	if !ok {
		return ""
	}
	return s.Discriminator.Mapping[name].Ref
}

// pointerEscaper escapes a JSON pointer reference token (RFC 6901).
var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")
