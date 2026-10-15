package openapivalidator

import (
	"bytes"
	"encoding/json"
	"errors"
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
// For an OpenAPI 3.1 document, ValidateResponse passes EnableJSONSchema2020,
// so VisitJSON (schema.go:2002-2009) first tries the JSON Schema 2020-12
// validator. That validator compiles the schema alone, as the resource
// https://example.com/schema.json, where no "#/components/..." reference
// resolves; compilation fails and kin falls back to visitJSON
// (openapi3/schema_jsonschema_validator.go:290-295). Every recursive schema
// holds a $ref, so it always reaches visitJSON and its skip. A kin release
// that resolves those references would check the nested values itself; the
// walk would then repeat kin's rows, which
// TestValidator_RecursiveSchema_DeepMismatchReported (exactly one row) shows.
//
// checkRecursive walks the decoded body alongside the response schema and
// keeps the same stack of schemas that kin keeps. Where the walk enters a
// schema that is already on the stack at an outer JSON pointer — where kin
// stops — it validates that sub-value with a fresh VisitJSON, and so a new
// visited set. It then continues into the sub-value with a stack that holds
// only that schema, which is the stack of the fresh visit. A re-entry at the
// same pointer is skipped, as kin skips it: the outer visit of that schema
// checks the same value.
//
// The walk enters only schemas from which a schema cycle is reachable
// (v.recursive); below any other schema no re-entry can happen. Each mismatch
// reads `recursive schema at "<pointer>": <kin error>`; the paths in the kin
// error are relative to that pointer.
//
// One defect gives one row. reported is the error kin's own ValidateResponse
// returned for the same body (nil if none). A walk mismatch whose absolute
// JSON pointer and reason equal those of a row in reported is dropped: it is
// kin's row again. This happens where the walk checks a union kin also
// checked, such as a union whose discriminator does not resolve. The result
// is nil or an openapi3.MultiError.
func (v *Validator) checkRecursive(route *routers.Route, req *http.Request, status int, header http.Header, body []byte, reported error) error {
	schema := responseSchema(route, v.opts, req, status, header)
	if schema == nil || !v.recursive[schema] {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // as openapi3filter.JSONBodyDecoder does
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil // kin reports the undecodable body
	}
	w := &recursiveWalk{
		recursive: v.recursive,
		opts:      schemaOptions(v.opts, route.Spec),
		checked:   map[reentry]bool{},
	}
	w.walk(schema, value, "", map[*openapi3.Schema]string{})

	seen := map[mismatchKey]bool{}
	if reported != nil {
		for _, e := range schemaErrors(reported) {
			if k, ok := keyOf("", e); ok {
				seen[k] = true
			}
		}
	}
	var errs openapi3.MultiError
	for _, m := range w.errs {
		if k, ok := keyOf(m.pointer, m.err); ok && seen[k] {
			continue
		}
		errs = append(errs, m)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// recursiveMismatch is one error a fresh check of the walk gave, for the
// value at pointer. Its text is `recursive schema at "<pointer>": <err>`.
type recursiveMismatch struct {
	pointer string
	err     error
}

func (m recursiveMismatch) Error() string {
	return fmt.Sprintf("recursive schema at %q: %v", m.pointer, m.err)
}

func (m recursiveMismatch) Unwrap() error { return m.err }

// mismatchKey identifies a schema defect: the absolute JSON pointer of the
// value and kin's reason.
type mismatchKey struct {
	pointer string
	reason  string
}

// keyOf returns the key of err, a kin error for the value at prefix, or false
// when err carries no *openapi3.SchemaError. The SchemaError's own path is
// relative to prefix.
func keyOf(prefix string, err error) (mismatchKey, bool) {
	var se *openapi3.SchemaError
	if !errors.As(err, &se) {
		return mismatchKey{}, false
	}
	p := prefix
	for _, token := range se.JSONPointer() {
		p += "/" + pointerEscaper.Replace(token)
	}
	return mismatchKey{pointer: p, reason: se.Reason}, true
}

// responseSchema returns the JSON body schema that openapi3filter.ValidateResponse
// validates for this response, or nil when it validates no JSON body
// (kin-openapi v0.149.0 openapi3filter/validate_response.go:22-120).
func responseSchema(route *routers.Route, opts *openapi3filter.Options, req *http.Request, status int, header http.Header) *openapi3.Schema {
	if req.Method == http.MethodHead || opts.ExcludeResponseBody {
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

// recursiveSchemas returns the schemas of doc's responses from which a schema
// cycle is reachable through the edges kin's visitJSON follows: allOf, oneOf,
// anyOf, prefixItems, items, properties and additionalProperties.
func recursiveSchemas(doc *openapi3.T) map[*openapi3.Schema]bool {
	const (
		visiting = 1
		done     = 2
	)
	state := map[*openapi3.Schema]int{}
	recursive := map[*openapi3.Schema]bool{}
	var visit func(s *openapi3.Schema) bool
	visit = func(s *openapi3.Schema) bool {
		if s == nil {
			return false
		}
		switch state[s] {
		case visiting:
			return true // a cycle back to a schema on the DFS path
		case done:
			return recursive[s]
		}
		state[s] = visiting
		found := false
		for _, child := range subschemas(s) {
			found = visit(child.Value) || found // finish every child
		}
		state[s] = done
		if found {
			recursive[s] = true
		}
		return found
	}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			for _, resp := range op.Responses.Map() {
				if resp.Value == nil {
					continue
				}
				for _, media := range resp.Value.Content {
					if media.Schema != nil {
						visit(media.Schema.Value)
					}
				}
			}
		}
	}
	return recursive
}

// subschemas returns the schemas kin's visitJSON descends into from s.
func subschemas(s *openapi3.Schema) openapi3.SchemaRefs {
	subs := slices.Concat(s.AllOf, s.OneOf, s.AnyOf, s.PrefixItems)
	if s.Items != nil {
		subs = append(subs, s.Items)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		subs = append(subs, s.Properties[name])
	}
	if s.AdditionalProperties.Schema != nil {
		subs = append(subs, s.AdditionalProperties.Schema)
	}
	return subs
}

// reentry identifies one fresh validation: a schema at a JSON pointer.
type reentry struct {
	schema  *openapi3.Schema
	pointer string
}

type recursiveWalk struct {
	recursive map[*openapi3.Schema]bool
	opts      []openapi3.SchemaValidationOption
	checked   map[reentry]bool // makes each fresh validation run once
	errs      []recursiveMismatch
}

// walk visits value with schema s, as kin's visitJSON does. stack maps each
// schema kin's visit has entered at this point to the pointer it entered it at.
func (w *recursiveWalk) walk(s *openapi3.Schema, value any, pointer string, stack map[*openapi3.Schema]string) {
	if s == nil || !w.recursive[s] {
		return
	}
	if at, ok := stack[s]; ok {
		if at == pointer {
			return // the outer visit of s checks this same value
		}
		w.check(s, value, pointer)
		stack = map[*openapi3.Schema]string{}
	}
	stack[s] = pointer
	defer delete(stack, s)

	for _, ref := range s.AllOf {
		w.walk(ref.Value, value, pointer, stack)
	}
	for _, ref := range w.oneOfMembers(s, value, pointer) {
		w.walk(ref.Value, value, pointer, stack)
	}
	if ref := w.anyOfMember(s, value, pointer); ref != nil {
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

// check validates value against s with a fresh VisitJSON, once per schema and
// pointer, and records each error.
func (w *recursiveWalk) check(s *openapi3.Schema, value any, pointer string) {
	key := reentry{s, pointer}
	if w.checked[key] {
		return
	}
	w.checked[key] = true
	if err := s.VisitJSON(value, w.opts...); err != nil {
		for _, e := range schemaErrors(err) {
			w.errs = append(w.errs, recursiveMismatch{pointer: pointer, err: e})
		}
	}
}

// oneOfMembers returns the oneOf members the walk follows: every candidate
// (see candidates) that the value validates against.
func (w *recursiveWalk) oneOfMembers(s *openapi3.Schema, value any, pointer string) openapi3.SchemaRefs {
	members, decided := w.candidates(s, s.OneOf, value)
	if decided {
		return members
	}
	members = slices.DeleteFunc(members, func(m *openapi3.SchemaRef) bool { return !w.accepts(m, value) })
	if len(members) == 0 {
		w.check(s, value, pointer) // no member accepts the value: the union fails
	}
	return members
}

// anyOfMember returns the anyOf member the walk follows: the first candidate
// (see candidates) that the value validates against, as kin stops at the
// first member that accepts (openapi3/schema.go:2365-2384).
func (w *recursiveWalk) anyOfMember(s *openapi3.Schema, value any, pointer string) *openapi3.SchemaRef {
	members, decided := w.candidates(s, s.AnyOf, value)
	if decided {
		if len(members) == 0 {
			return nil
		}
		return members[0]
	}
	for _, m := range members {
		if w.accepts(m, value) {
			return m
		}
	}
	w.check(s, value, pointer) // no member accepts the value: the union fails
	return nil
}

// candidates returns the union members kin considers: with a discriminator
// mapping that names the value's discriminator, only the mapped member
// (openapi3/schema.go:2234-2272, and the comparison at :2294 for oneOf and
// :2357 for anyOf); otherwise every member. decided reports that no fresh
// check is needed: there is at most one candidate, or no candidate can reach a
// schema cycle, so kin's own check of the members is complete.
func (w *recursiveWalk) candidates(s *openapi3.Schema, members openapi3.SchemaRefs, value any) (_ openapi3.SchemaRefs, decided bool) {
	members = slices.Clone(members)
	if mapped := mappedRef(s, value); mapped != "" {
		members = slices.DeleteFunc(members, func(m *openapi3.SchemaRef) bool { return m.Ref != mapped })
	}
	if len(members) <= 1 {
		return members, true
	}
	if !slices.ContainsFunc(members, func(m *openapi3.SchemaRef) bool { return w.recursive[m.Value] }) {
		return nil, true
	}
	return members, false
}

// accepts reports whether a fresh VisitJSON of member accepts value.
func (w *recursiveWalk) accepts(member *openapi3.SchemaRef, value any) bool {
	return member.Value != nil && member.Value.VisitJSON(value, w.opts...) == nil
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
