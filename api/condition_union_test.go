package api

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const schemaRefPrefix = "#/components/schemas/"

// The wire values of a condition's `type`, as the condition parser reads
// them, and the schema each one selects.
var (
	criterionUnionMapping = map[string]string{
		"array":     schemaRefPrefix + "ArrayConditionDto",
		"group":     schemaRefPrefix + "CriterionGroupConditionDto",
		"function":  schemaRefPrefix + "FunctionConditionDto",
		"lifecycle": schemaRefPrefix + "LifecycleConditionDto",
		"simple":    schemaRefPrefix + "SimpleConditionDto",
	}
	localUnionMapping = map[string]string{
		"array":     schemaRefPrefix + "ArrayConditionDto",
		"group":     schemaRefPrefix + "GroupConditionDto",
		"lifecycle": schemaRefPrefix + "LifecycleConditionDto",
		"simple":    schemaRefPrefix + "SimpleConditionDto",
	}
)

func propertyOf(t *testing.T, s *openapi3.Schema, name string) *openapi3.Schema {
	t.Helper()
	if p := s.Properties[name]; p != nil && p.Value != nil {
		return p.Value
	}
	for _, part := range s.AllOf {
		if p := part.Value.Properties[name]; p != nil && p.Value != nil {
			return p.Value
		}
	}
	t.Fatalf("property %q missing", name)
	return nil
}

func requestBodySchema(t *testing.T, doc *openapi3.T, path string) *openapi3.Schema {
	t.Helper()
	item := doc.Paths.Find(path)
	if item == nil || item.Post == nil || item.Post.RequestBody == nil {
		t.Fatalf("POST %s has no request body", path)
	}
	return item.Post.RequestBody.Value.Content.Get("application/json").Schema.Value
}

// Every condition union names its member by the condition's `type`, so a
// validator checks a condition against the one schema its `type` selects.
func TestConditionUnionsDiscriminateByType(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	schema := func(name string) *openapi3.Schema { return doc.Components.Schemas[name].Value }
	unions := []struct {
		where string
		s     *openapi3.Schema
		want  map[string]string
	}{
		{"WorkflowConfigurationDto.criterion", propertyOf(t, schema("WorkflowConfigurationDto"), "criterion"), criterionUnionMapping},
		{"TransitionDefinitionDto.criterion", propertyOf(t, schema("TransitionDefinitionDto"), "criterion"), criterionUnionMapping},
		{"CriterionGroupConditionDto.conditions[]", propertyOf(t, schema("CriterionGroupConditionDto"), "conditions").Items.Value, criterionUnionMapping},
		{"ExternalizedFunctionDto.criterion", propertyOf(t, schema("ExternalizedFunctionDto"), "criterion"), localUnionMapping},
		{"GroupConditionDto.conditions[]", propertyOf(t, schema("GroupConditionDto"), "conditions").Items.Value, localUnionMapping},
		{"GroupedStatsRequest.condition", propertyOf(t, schema("GroupedStatsRequest"), "condition"), localUnionMapping},
		{"POST /search/async body", requestBodySchema(t, doc, "/search/async/{entityName}/{modelVersion}"), localUnionMapping},
		{"POST /search/direct body", requestBodySchema(t, doc, "/search/direct/{entityName}/{modelVersion}"), localUnionMapping},
	}
	for _, u := range unions {
		d := u.s.Discriminator
		if d == nil {
			t.Errorf("%s: no discriminator", u.where)
			continue
		}
		if d.PropertyName != "type" {
			t.Errorf("%s: discriminator property %q, want type", u.where, d.PropertyName)
		}
		got := map[string]string{}
		for k, v := range d.Mapping {
			got[k] = v.Ref
		}
		if !maps.Equal(got, u.want) {
			t.Errorf("%s: discriminator mapping %v, want %v", u.where, got, u.want)
		}
		members := map[string]bool{}
		for _, b := range u.s.OneOf {
			members[b.Ref] = true
		}
		for _, ref := range u.want {
			if !members[ref] {
				t.Errorf("%s: mapping names %s, which is not a oneOf member", u.where, ref)
			}
		}
	}
}

// Exported workflow criteria validate as the server writes them, and a
// shape the server refuses does not validate. Each case is checked with the
// default validator (the e2e response validator) and the JSON Schema
// 2020-12 one.
func TestCriterionShapesValidate(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	transition := doc.Components.Schemas["TransitionDefinitionDto"].Value
	searchGroup := doc.Components.Schemas["GroupConditionDto"].Value
	simple := doc.Components.Schemas["SimpleConditionDto"].Value
	lifecycle := doc.Components.Schemas["LifecycleConditionDto"].Value
	tr := func(criterion string) string {
		return `{"name":"p","next":"X","manual":false,"criterion":` + criterion + `}`
	}
	cases := []struct {
		name  string
		s     *openapi3.Schema
		value string
		ok    bool
	}{
		{"group with a function member and no pre-check", transition, tr(`{"type":"group","operator":"AND","conditions":[
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10},
			{"type":"group","operator":"NOT","conditions":[{"type":"function","function":{"name":"a","criterion":
				{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}}}]},
			{"type":"function","function":{"name":"b"}}]}`), true},
		{"function with a null pre-check", transition, tr(`{"type":"function","function":{"name":"f","criterion":null}}`), true},
		{"function with a group pre-check", transition, tr(`{"type":"function","function":{"name":"f","criterion":
			{"type":"group","operator":"OR","conditions":[{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":["a"]}]}}}`), true},
		{"simple criterion with a scalar value", transition, tr(`{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1}`), true},
		{"function in a pre-check", transition, tr(`{"type":"function","function":{"name":"f","criterion":{"type":"function","function":{"name":"g"}}}}`), false},
		{"unknown condition type", transition, tr(`{"type":"bogus"}`), false},
		{"simple without jsonPath", transition, tr(`{"type":"simple","operatorType":"EQUALS","value":1}`), false},
		{"function member in a search group", searchGroup, `{"type":"group","operator":"AND","conditions":[{"type":"function","function":{"name":"f"}}]}`, false},
		{"simple value number", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1.5}`, true},
		{"simple value string", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":"x"}`, true},
		{"simple value boolean", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":true}`, true},
		{"simple value array", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"BETWEEN","value":[1,2]}`, true},
		{"simple value null", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"IS_NULL","value":null}`, true},
		{"simple without value", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"IS_NULL"}`, true},
		{"simple value object", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":{"k":1}}`, false},
		{"simple value three-element array", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"BETWEEN","value":[1,2,3]}`, false},
		{"simple value pair with a null bound", simple, `{"type":"simple","jsonPath":"$.a","operatorType":"BETWEEN","value":[1,null]}`, false},
		{"lifecycle value string", lifecycle, `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`, true},
		{"lifecycle value null", lifecycle, `{"type":"lifecycle","field":"previousTransition","operatorType":"IS_NULL","value":null}`, true},
		{"lifecycle without value", lifecycle, `{"type":"lifecycle","field":"state","operatorType":"NOT_NULL"}`, true},
		{"lifecycle value object", lifecycle, `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":{"k":1}}`, false},
	}
	for _, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.value), &v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for mode, opts := range schemaValidators {
			err := c.s.VisitJSON(v, opts...)
			if c.ok && err != nil {
				t.Errorf("%s: %s refused: %v", mode, c.name, err)
			}
			if !c.ok && err == nil {
				t.Errorf("%s: %s accepted", mode, c.name)
			}
		}
	}
}
