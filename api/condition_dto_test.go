package api

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/getkin/kin-openapi/openapi3"
)

// The operator enum each condition DTO publishes is the set the server
// accepts — no more (a listed name the server refuses misleads every
// generated client) and no less.
func TestConditionDtoOperatorEnumsMatchServer(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	want := spi.OperatorNames()
	for _, dto := range []string{"ArrayConditionDto", "SimpleConditionDto", "LifecycleConditionDto"} {
		s := doc.Components.Schemas[dto]
		if s == nil || s.Value == nil {
			t.Fatalf("%s missing", dto)
		}
		var op *openapi3.SchemaRef
		for _, part := range s.Value.AllOf {
			if p := part.Value.Properties["operatorType"]; p != nil {
				op = p
			}
		}
		if op == nil || op.Value == nil {
			t.Fatalf("%s.operatorType missing", dto)
		}
		var got []string
		for _, v := range op.Value.Enum {
			got = append(got, v.(string))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s.operatorType enum = %v, want %v", dto, got, want)
		}
	}
}

func TestArrayConditionDtoShape(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	s := doc.Components.Schemas["ArrayConditionDto"].Value
	props := map[string]bool{}
	required := map[string]bool{}
	for _, part := range s.AllOf {
		for k := range part.Value.Properties {
			props[k] = true
		}
		for _, r := range part.Value.Required {
			required[r] = true
		}
	}
	for _, r := range s.Required {
		required[r] = true
	}
	for _, want := range []string{"jsonPath", "operatorType", "values"} {
		if !props[want] || !required[want] {
			t.Errorf("ArrayConditionDto.%s must be a required property", want)
		}
	}
	if props["value"] {
		t.Error("ArrayConditionDto must not declare `value`; the server reads `values`")
	}

	values := arrayConditionValuesSchema(t, doc)
	if values.MinItems != 1 {
		t.Errorf("ArrayConditionDto.values minItems = %d, want 1", values.MinItems)
	}
	// A whole condition tests at most 10,000 entries (search.MaxConditionEntries),
	// so one clause's values cannot hold more.
	if values.MaxItems == nil || *values.MaxItems != 10000 {
		t.Errorf("ArrayConditionDto.values maxItems = %v, want 10000", values.MaxItems)
	}
	if !strings.Contains(values.Description, "10,000") {
		t.Errorf("ArrayConditionDto.values description must state the 10,000-entry cap, got %q", values.Description)
	}
	items := values.Items
	if items == nil || items.Value == nil {
		t.Fatal("ArrayConditionDto.values has no item schema")
	}
	var hasNull, hasPair bool
	for _, branch := range items.Value.OneOf {
		b := branch.Value
		if b.Type.Is("null") {
			hasNull = true
		}
		if b.Type.Is("array") {
			max := uint64(0)
			if b.MaxItems != nil {
				max = *b.MaxItems
			}
			if b.MinItems != 2 || max != 2 {
				t.Errorf("ArrayConditionDto.values pair branch has minItems %d, maxItems %d; want 2 and 2", b.MinItems, max)
			}
			hasPair = true
		}
	}
	if !hasNull {
		t.Error("ArrayConditionDto.values items need an OpenAPI 3.1 `null` branch: a null entry skips its position")
	}
	if !hasPair {
		t.Error("ArrayConditionDto.values items need a [low, high] pair branch")
	}
}

func arrayConditionValuesSchema(t *testing.T, doc *openapi3.T) *openapi3.Schema {
	t.Helper()
	for _, part := range doc.Components.Schemas["ArrayConditionDto"].Value.AllOf {
		if p := part.Value.Properties["values"]; p != nil && p.Value != nil {
			return p.Value
		}
	}
	t.Fatal("ArrayConditionDto.values missing")
	return nil
}

// The spec declares OpenAPI 3.1, where `nullable` is not a keyword: a null
// value is admitted only by a `null` type. Each value is checked with both
// kin-openapi validators — the default one the e2e response validator uses,
// and the JSON Schema 2020-12 one a 3.1 consumer applies.
var schemaValidators = map[string][]openapi3.SchemaValidationOption{
	"default":             nil,
	"json-schema-2020-12": {openapi3.EnableJSONSchema2020()},
}

func TestArrayConditionDtoValuesValidateUnderOpenAPI31(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	values := arrayConditionValuesSchema(t, doc)
	whole := doc.Components.Schemas["ArrayConditionDto"].Value
	for mode, opts := range schemaValidators {
		if err := whole.VisitJSON(whole.Example, opts...); err != nil {
			t.Errorf("%s: the ArrayConditionDto example does not validate against its schema: %v", mode, err)
		}
		if err := values.VisitJSON([]any{50.0, nil, 70.0}, opts...); err != nil {
			t.Errorf("%s: values [50, null, 70] refused: %v", mode, err)
		}
		if err := values.VisitJSON([]any{[]any{1.0, 2.0}, nil}, opts...); err != nil {
			t.Errorf("%s: values [[1, 2], null] refused: %v", mode, err)
		}
		for name, bad := range map[string][]any{
			"a three-element pair":  {[]any{1.0, 2.0, 3.0}},
			"an object entry":       {map[string]any{"x": 1.0}},
			"no entries":            {},
			"a pair of non-scalars": {[]any{map[string]any{}, []any{}}},
		} {
			if err := values.VisitJSON(bad, opts...); err == nil {
				t.Errorf("%s: values with %s %v accepted", mode, name, bad)
			}
		}
	}
}

func TestSpecDeclaresNoNullableKeyword(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	seen := map[*openapi3.Schema]bool{}
	var walk func(where string, ref *openapi3.SchemaRef)
	walk = func(where string, ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		s := ref.Value
		seen[s] = true
		if s.Nullable {
			t.Errorf("%s uses `nullable`, which OpenAPI 3.1 ignores; add a `null` type", where)
		}
		for name, p := range s.Properties {
			walk(where+"."+name, p)
		}
		walk(where+"[]", s.Items)
		if s.AdditionalProperties.Schema != nil {
			walk(where+"{}", s.AdditionalProperties.Schema)
		}
		for _, group := range []openapi3.SchemaRefs{s.AllOf, s.OneOf, s.AnyOf} {
			for _, b := range group {
				walk(where, b)
			}
		}
		walk(where+"!", s.Not)
	}
	for name, ref := range doc.Components.Schemas {
		walk(name, ref)
	}

	// The nullable values the spec documents still admit null.
	nullable := map[string]*openapi3.Schema{
		"ExternalizedFunctionDto.criterion": doc.Components.Schemas["ExternalizedFunctionDto"].Value.
			Properties["criterion"].Value,
		"GroupKeyEntry.value": doc.Components.Schemas["GroupKeyEntry"].Value.Properties["value"].Value,
		"GroupedStatsBucket.aggregations{}": doc.Components.Schemas["GroupedStatsBucket"].Value.
			Properties["aggregations"].Value.AdditionalProperties.Schema.Value,
	}
	for _, part := range doc.Components.Schemas["StateMachineAuditEventDto"].Value.AllOf {
		if p := part.Value.Properties["data"]; p != nil {
			nullable["StateMachineAuditEventDto.data"] = p.Value
		}
	}
	if len(nullable) != 4 {
		t.Fatalf("expected four nullable values, found %v", nullable)
	}
	for where, s := range nullable {
		for mode, opts := range schemaValidators {
			if err := s.VisitJSON(nil, opts...); err != nil {
				t.Errorf("%s: %s refuses null: %v", mode, where, err)
			}
		}
	}
}

// An operation description that lists the predicate operators names only
// operators the server accepts: a listed name the server refuses with 400
// INVALID_CONDITION is documentation no client can follow.
func TestOperationDescriptionsListOnlyAcceptedOperators(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	accepted := spi.OperatorNames()
	entry := regexp.MustCompile(`^\s*- ([A-Z_]+(?:, [A-Z_]+)*):`)
	checked := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !strings.Contains(op.Description, "Operator Types") {
				continue
			}
			checked++
			for _, line := range strings.Split(op.Description, "\n") {
				m := entry.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				for _, name := range strings.Split(m[1], ", ") {
					if !slices.Contains(accepted, name) {
						t.Errorf("%s %s description lists operator %s, which the server refuses", method, path, name)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operation description lists the operators; the test checks nothing")
	}
}
