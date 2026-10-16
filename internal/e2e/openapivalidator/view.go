package openapivalidator

import (
	"fmt"
	"sort"
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
//  2. kin refuses null for an untyped leaf. Each untyped leaf is marked
//     nullable, unless its not excludes null.
//  3. kin accepts any value for a schema it is already visiting higher up, so
//     null reaching a recursive member through a nullable reference
//     (oneOf [{type: null}, {$ref}]) matches both branches and is refused.
//     Each union with a {type: null} branch is marked nullable, which
//     accepts null before the branches are tried; it accepted null already.
//
// No other schema is marked, so a member and a replaced use keep refusing
// null.
//
// The document is changed in place; callers pass one that api.GetSwagger just
// parsed.
func ApplyView(doc *openapi3.T) error {
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
		for _, name := range sortedKeys(doc.Components.Schemas) {
			v.walk(doc.Components.Schemas[name].Value)
		}
		for _, p := range doc.Components.Parameters {
			v.parameterRef(p)
		}
		for _, h := range doc.Components.Headers {
			if h != nil && h.Value != nil {
				v.parameter(&h.Value.Parameter)
			}
		}
		for _, b := range doc.Components.RequestBodies {
			v.requestBodyRef(b)
		}
		for _, r := range doc.Components.Responses {
			v.responseRef(r)
		}
	}
	if doc.Paths != nil {
		for _, item := range doc.Paths.Map() {
			for _, p := range item.Parameters {
				v.parameterRef(p)
			}
			for _, op := range item.Operations() {
				for _, p := range op.Parameters {
					v.parameterRef(p)
				}
				v.requestBodyRef(op.RequestBody)
				if op.Responses != nil {
					for _, r := range op.Responses.Map() {
						v.responseRef(r)
					}
				}
			}
		}
	}
	return v.err
}

type viewBuilder struct {
	doc     *openapi3.T
	bases   map[string]*openapi3.Schema // "#/components/schemas/X" -> base schema
	visited map[*openapi3.Schema]bool
	err     error
}

func sortedKeys(m openapi3.Schemas) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (v *viewBuilder) parameterRef(p *openapi3.ParameterRef) {
	if p != nil && p.Value != nil {
		v.parameter(p.Value)
	}
}

func (v *viewBuilder) parameter(p *openapi3.Parameter) {
	p.Schema = v.use(p.Schema)
	v.content(p.Content)
}

func (v *viewBuilder) requestBodyRef(b *openapi3.RequestBodyRef) {
	if b != nil && b.Value != nil {
		v.content(b.Value.Content)
	}
}

func (v *viewBuilder) responseRef(r *openapi3.ResponseRef) {
	if r == nil || r.Value == nil {
		return
	}
	v.content(r.Value.Content)
	for _, h := range r.Value.Headers {
		if h != nil && h.Value != nil {
			v.parameter(&h.Value.Parameter)
		}
	}
}

func (v *viewBuilder) content(c openapi3.Content) {
	for _, mt := range c {
		if mt != nil {
			mt.Schema = v.use(mt.Schema)
		}
	}
}

// use returns the schema reference to store in a slot that holds ref: a oneOf
// of the mapping targets when ref names a base, otherwise ref itself, whose
// schema is walked.
func (v *viewBuilder) use(ref *openapi3.SchemaRef) *openapi3.SchemaRef {
	if ref == nil {
		return nil
	}
	if base, ok := v.bases[ref.Ref]; ok {
		return v.replacement(ref.Ref, base)
	}
	v.walk(ref.Value)
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
		member, ok := v.doc.Components.Schemas[strings.TrimPrefix(target, schemaRefPrefix)]
		if !ok || !strings.HasPrefix(target, schemaRefPrefix) {
			if v.err == nil {
				v.err = fmt.Errorf("discriminator mapping of %s names %q, which is not a component schema", baseRef, target)
			}
			continue
		}
		oneOf = append(oneOf, &openapi3.SchemaRef{Ref: target, Value: member.Value})
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{OneOf: oneOf, Discriminator: base.Discriminator}}
}

// walk visits s once. It replaces uses of bases below s and marks s nullable
// when it is an untyped leaf.
func (v *viewBuilder) walk(s *openapi3.Schema) {
	if s == nil || v.visited[s] {
		return
	}
	v.visited[s] = true

	for name, p := range s.Properties {
		s.Properties[name] = v.use(p)
	}
	s.Items = v.use(s.Items)
	s.AdditionalProperties.Schema = v.use(s.AdditionalProperties.Schema)
	s.Not = v.use(s.Not)
	for _, m := range s.AllOf { // a member's allOf entry keeps its $ref to the base
		if m != nil {
			v.walk(m.Value)
		}
	}
	for i, m := range s.OneOf {
		s.OneOf[i] = v.use(m)
	}
	for i, m := range s.AnyOf {
		s.AnyOf[i] = v.use(m)
	}

	if isUntypedLeaf(s) && !notExcludesNull(s) || hasNullBranch(s) {
		s.Nullable = true
	}
}

func isUntypedLeaf(s *openapi3.Schema) bool {
	return (s.Type == nil || len(*s.Type) == 0) &&
		len(s.AllOf) == 0 && len(s.OneOf) == 0 && len(s.AnyOf) == 0
}

func hasNullBranch(s *openapi3.Schema) bool {
	for _, union := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf} {
		for _, b := range union {
			if b != nil && b.Ref == "" && b.Value != nil && b.Value.Type != nil && b.Value.Type.Is(openapi3.TypeNull) {
				return true
			}
		}
	}
	return false
}

func notExcludesNull(s *openapi3.Schema) bool {
	return s.Not != nil && s.Not.Value != nil && s.Not.Value.Type != nil && s.Not.Value.Type.IncludesNull()
}
