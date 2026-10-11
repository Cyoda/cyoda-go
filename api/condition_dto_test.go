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
