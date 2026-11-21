package openapivalidator

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

const schemaRefPrefix = "#/components/schemas/"

// ApplyView rewrites a freshly loaded document in place so that kin-openapi
// gives JSON Schema's answers where it otherwise would not:
//
//  1. kin checks a value against a base's own properties only and follows a
//     discriminator mapping only on a oneOf. Each use of a base (a schema with
//     a discriminator and a mapping) is therefore replaced by a oneOf of the
//     base's mapping targets that carries the base's discriminator. Each member
//     $ref is spelled as in the mapping. A member's allOf entry is left alone.
//     kin's JSON Schema 2020-12 validator knows no discriminator, so each
//     mapping target also requires the discriminator property and limits it
//     to the keys that map to it: there the oneOf matches only the member the
//     key names. Through a use of the base, kin's own visitor checks only the
//     member the key selects, which the limit admits. A member used directly
//     takes only its own keys. A target in two mappings, or one that is itself
//     a base, cannot be limited: ApplyView returns an error that names it.
//  2. kin refuses null for an untyped leaf: for a 3.1 document it ignores
//     nullable, which 3.1 does not have, and accepts null only for a schema
//     whose type lists it. Each untyped leaf is given every JSON type, unless
//     its not excludes null. An untyped schema accepts every type, so listing
//     all seven changes no other value's answer. kin accepts null before it
//     checks enum and const, so a leaf whose answer for null depends on an
//     enum, a const or a not beyond a type is one the view cannot decide:
//     ApplyView returns an error that names it.
//
// The leaves of point 2 are the only schemas given null, so a member and a
// replaced use keep refusing it.
//
// The document is changed in place; callers pass one that api.GetSwagger just
// parsed.
func ApplyView(doc *openapi3.T) error {
	v := newViewBuilder(doc)
	v.limitMembersToTheirKeys()
	if doc.Components != nil {
		for _, name := range sortedKeys(doc.Components.Schemas) {
			v.walk(doc.Components.Schemas[name].Value, name)
		}
		for name, p := range doc.Components.Parameters {
			v.parameterRef(p, "parameter "+name)
		}
		for name, h := range doc.Components.Headers {
			if h != nil && h.Value != nil {
				v.parameter(&h.Value.Parameter, "header "+name)
			}
		}
		for name, b := range doc.Components.RequestBodies {
			v.requestBodyRef(b, "requestBody "+name)
		}
		for name, r := range doc.Components.Responses {
			v.responseRef(r, "response "+name)
		}
	}
	if doc.Paths != nil {
		for path, item := range doc.Paths.Map() {
			for _, p := range item.Parameters {
				v.parameterRef(p, path+" param")
			}
			for method, op := range item.Operations() {
				where := method + " " + path
				for _, p := range op.Parameters {
					v.parameterRef(p, where+" param")
				}
				v.requestBodyRef(op.RequestBody, where+" body")
				if op.Responses != nil {
					for code, r := range op.Responses.Map() {
						v.responseRef(r, where+" "+code)
					}
				}
			}
		}
	}
	return v.err
}

// ComponentUse returns the schema that a $ref to the component schema name
// stands for in the view: the oneOf of the mapping targets when name is a
// base, otherwise the component's schema. doc is in the view already.
func ComponentUse(doc *openapi3.T, name string) (*openapi3.Schema, error) {
	var ref *openapi3.SchemaRef
	if doc.Components != nil {
		ref = doc.Components.Schemas[name]
	}
	if ref == nil || ref.Value == nil {
		return nil, fmt.Errorf("%q is not a component schema", name)
	}
	v := newViewBuilder(doc)
	if base, ok := v.bases[schemaRefPrefix+name]; ok {
		use := v.replacement(schemaRefPrefix+name, base)
		return use.Value, v.err
	}
	return ref.Value, nil
}

type viewBuilder struct {
	doc     *openapi3.T
	bases   map[string]*openapi3.Schema // "#/components/schemas/X" -> base schema
	visited map[*openapi3.Schema]bool
	err     error
}

// newViewBuilder returns a builder that knows doc's bases: the component
// schemas with a discriminator and a mapping and no oneOf or anyOf.
func newViewBuilder(doc *openapi3.T) *viewBuilder {
	v := &viewBuilder{
		doc:     doc,
		bases:   map[string]*openapi3.Schema{},
		visited: map[*openapi3.Schema]bool{},
	}
	if doc.Components != nil {
		for name, ref := range doc.Components.Schemas {
			s := ref.Value
			if s != nil && s.Discriminator != nil && len(s.Discriminator.Mapping) > 0 &&
				len(s.OneOf) == 0 && len(s.AnyOf) == 0 {
				v.bases[schemaRefPrefix+name] = s
			}
		}
	}
	return v
}

func sortedKeys(m openapi3.Schemas) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (v *viewBuilder) parameterRef(p *openapi3.ParameterRef, where string) {
	if p != nil && p.Value != nil {
		v.parameter(p.Value, where+" "+p.Value.Name)
	}
}

func (v *viewBuilder) parameter(p *openapi3.Parameter, where string) {
	p.Schema = v.use(p.Schema, where)
	v.content(p.Content, where)
}

func (v *viewBuilder) requestBodyRef(b *openapi3.RequestBodyRef, where string) {
	if b != nil && b.Value != nil {
		v.content(b.Value.Content, where)
	}
}

func (v *viewBuilder) responseRef(r *openapi3.ResponseRef, where string) {
	if r == nil || r.Value == nil {
		return
	}
	v.content(r.Value.Content, where)
	for name, h := range r.Value.Headers {
		if h != nil && h.Value != nil {
			v.parameter(&h.Value.Parameter, where+" header "+name)
		}
	}
}

func (v *viewBuilder) content(c openapi3.Content, where string) {
	for name, mt := range c {
		if mt != nil {
			mt.Schema = v.use(mt.Schema, where+" "+name)
		}
	}
}

// use returns the schema reference to store in a slot that holds ref: a oneOf
// of the mapping targets when ref names a base, otherwise ref itself, whose
// schema is walked. where names the slot; a $ref to a component schema is
// named by the component.
func (v *viewBuilder) use(ref *openapi3.SchemaRef, where string) *openapi3.SchemaRef {
	if ref == nil {
		return nil
	}
	if base, ok := v.bases[ref.Ref]; ok {
		return v.replacement(ref.Ref, base)
	}
	if name, ok := strings.CutPrefix(ref.Ref, schemaRefPrefix); ok {
		where = name
	}
	v.walk(ref.Value, where)
	return ref
}

func (v *viewBuilder) replacement(baseRef string, base *openapi3.Schema) *openapi3.SchemaRef {
	targets := make([]string, 0, len(base.Discriminator.Mapping))
	for _, m := range base.Discriminator.Mapping {
		targets = append(targets, m.Ref)
	}
	sort.Strings(targets)
	oneOf := make(openapi3.SchemaRefs, 0, len(targets))
	for _, target := range targets {
		member, ok := v.member(baseRef, target)
		if !ok {
			continue
		}
		oneOf = append(oneOf, &openapi3.SchemaRef{Ref: target, Value: member})
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{OneOf: oneOf, Discriminator: base.Discriminator}}
}

// member returns the component schema that target, a mapping target of the
// base baseRef, names, or records an error.
func (v *viewBuilder) member(baseRef, target string) (*openapi3.Schema, bool) {
	ref, ok := v.doc.Components.Schemas[strings.TrimPrefix(target, schemaRefPrefix)]
	if !ok || !strings.HasPrefix(target, schemaRefPrefix) || ref.Value == nil {
		if v.err == nil {
			v.err = fmt.Errorf("discriminator mapping of %s names %q, which is not a component schema", baseRef, target)
		}
		return nil, false
	}
	return ref.Value, true
}

// limitMembersToTheirKeys adds to each mapping target of each base an allOf
// entry that requires the discriminator property and limits it to the keys
// that map to the target. A target in the mappings of two bases, or one that
// is itself a base, is a shape it cannot limit: it records an error.
func (v *viewBuilder) limitMembersToTheirKeys() {
	limited := map[string]string{} // target -> the base whose keys limit it
	for _, baseRef := range slices.Sorted(maps.Keys(v.bases)) {
		d := v.bases[baseRef].Discriminator
		keys := map[string][]any{} // target -> the keys that map to it
		for _, key := range slices.Sorted(maps.Keys(d.Mapping)) {
			keys[d.Mapping[key].Ref] = append(keys[d.Mapping[key].Ref], key)
		}
		for _, target := range slices.Sorted(maps.Keys(keys)) {
			member, ok := v.member(baseRef, target)
			if !ok {
				continue
			}
			if other, ok := limited[target]; ok || v.bases[target] != nil {
				if v.err == nil {
					v.err = fmt.Errorf("mapping target %s is a base or in the mapping of another base (%s); the view cannot limit it to the keys of %s", target, other, baseRef)
				}
				continue
			}
			limited[target] = baseRef
			member.AllOf = append(member.AllOf, &openapi3.SchemaRef{Value: &openapi3.Schema{
				Required: []string{d.PropertyName},
				Properties: openapi3.Schemas{d.PropertyName: {Value: &openapi3.Schema{
					Type: &openapi3.Types{openapi3.TypeString}, Enum: keys[target],
				}}},
			}})
		}
	}
}

// walk visits s, which where names, once. It replaces uses of bases below s
// and gives s every JSON type when it is an untyped leaf that admits null.
func (v *viewBuilder) walk(s *openapi3.Schema, where string) {
	if s == nil || v.visited[s] {
		return
	}
	v.visited[s] = true

	for name, p := range s.Properties {
		s.Properties[name] = v.use(p, where+"."+name)
	}
	s.Items = v.use(s.Items, where+"[]")
	s.AdditionalProperties.Schema = v.use(s.AdditionalProperties.Schema, where+"{}")
	s.Not = v.use(s.Not, where+"!")
	for i, m := range s.AllOf { // a member's allOf entry keeps its $ref to the base
		if m != nil {
			v.walk(m.Value, where+"/allOf/"+strconv.Itoa(i))
		}
	}
	for i, m := range s.OneOf {
		s.OneOf[i] = v.use(m, where+"/oneOf/"+strconv.Itoa(i))
	}
	for i, m := range s.AnyOf {
		s.AnyOf[i] = v.use(m, where+"/anyOf/"+strconv.Itoa(i))
	}

	if !isUntypedLeaf(s) {
		return
	}
	if !nullDecidedByNotType(s) {
		if v.err == nil {
			v.err = fmt.Errorf("untyped schema %s has an enum, a const or a not beyond a type; the view cannot decide whether it admits null", where)
		}
		return
	}
	if !notExcludesNull(s) {
		s.Type = &openapi3.Types{
			openapi3.TypeArray, openapi3.TypeBoolean, openapi3.TypeInteger, openapi3.TypeNull,
			openapi3.TypeNumber, openapi3.TypeObject, openapi3.TypeString,
		}
	}
}

// nullDecidedByNotType reports whether nothing but the type of its not decides
// whether the untyped leaf s admits null: s has no enum and no const, and its
// not, if any, holds a type and no other assertion.
func nullDecidedByNotType(s *openapi3.Schema) bool {
	if len(s.Enum) > 0 || s.Const != nil {
		return false
	}
	if s.Not == nil || s.Not.Value == nil {
		return true
	}
	rest := *s.Not.Value
	rest.Type = nil
	return rest.IsEmpty()
}

// isUntypedLeaf reports whether s has no type, no union or allOf, and is not
// a boolean schema (true or false), which decides every value itself.
func isUntypedLeaf(s *openapi3.Schema) bool {
	return (s.Type == nil || len(*s.Type) == 0) && s.Always == nil &&
		len(s.AllOf) == 0 && len(s.OneOf) == 0 && len(s.AnyOf) == 0
}

func notExcludesNull(s *openapi3.Schema) bool {
	return s.Not != nil && s.Not.Value != nil && s.Not.Value.Type != nil && s.Not.Value.Type.IncludesNull()
}
