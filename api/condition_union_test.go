package api

import (
	"encoding/json"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// viewedSpec returns the spec through the validator view, which gives JSON
// Schema's answers where kin-openapi otherwise would not: it follows a base's
// mapping at every use, and accepts null for an untyped leaf.
func viewedSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	if err := openapivalidator.ApplyView(doc); err != nil {
		t.Fatalf("ApplyView: %v", err)
	}
	return doc
}

func requestBodySchema(t *testing.T, doc *openapi3.T, method, path string) *openapi3.Schema {
	t.Helper()
	item := doc.Paths.Find(path)
	if item == nil || item.GetOperation(method) == nil || item.GetOperation(method).RequestBody == nil {
		t.Fatalf("%s %s has no request body", method, path)
	}
	return item.GetOperation(method).RequestBody.Value.Content.Get("application/json").Schema.Value
}

// validateCases checks each JSON value against s, a schema of doc, in both
// modes of schemaValidators.
func validateCases(t *testing.T, doc *openapi3.T, cases []validateCase) {
	t.Helper()
	for _, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.value), &v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for mode, opts := range schemaValidators {
			err := openapivalidator.ValidateValue(doc, c.s, v, opts...)
			if c.ok && err != nil {
				t.Errorf("%s: %s refused: %v", mode, c.name, err)
			}
			if !c.ok && err == nil {
				t.Errorf("%s: %s accepted", mode, c.name)
			}
		}
	}
}

type validateCase struct {
	name  string
	s     *openapi3.Schema
	value string
	ok    bool
}

// Every condition position selects its member by the condition's `type`, so
// a condition is checked against the one schema its `type` names.
func TestConditionUnionsDiscriminateByType(t *testing.T) {
	doc := viewedSpec(t)
	schema := func(name string) *openapi3.Schema { return doc.Components.Schemas[name].Value }
	prop := func(s *openapi3.Schema, name string) *openapi3.Schema {
		p := ownProperty(s, name)
		if p == nil || p.Value == nil {
			t.Fatalf("property %s missing", name)
		}
		return p.Value
	}
	positions := map[string]*openapi3.Schema{
		"WorkflowConfigurationDto.criterion": prop(schema("WorkflowConfigurationDto"), "criterion"),
		"TransitionDefinitionDto.criterion":  prop(schema("TransitionDefinitionDto"), "criterion"),
		"ExternalizedFunctionDto.criterion":  prop(schema("ExternalizedFunctionDto"), "criterion"),
		"GroupConditionDto.conditions[]":     prop(schema("GroupConditionDto"), "conditions").Items.Value,
		"GroupedStatsRequest.condition":      prop(schema("GroupedStatsRequest"), "condition"),
		"POST /search/async body":            requestBodySchema(t, doc, "POST", "/search/async/{entityName}/{modelVersion}"),
		"POST /search/direct body":           requestBodySchema(t, doc, "POST", "/search/direct/{entityName}/{modelVersion}"),
		"DELETE /entity body":                requestBodySchema(t, doc, "DELETE", "/entity/{entityName}/{modelVersion}"),
	}
	var cases []validateCase
	for where, s := range positions {
		cases = append(cases,
			validateCase{where + ": simple", s, `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1}`, true},
			validateCase{where + ": lifecycle", s, `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"A"}`, true},
			validateCase{where + ": array", s, `{"type":"array","jsonPath":"$.a[*]","operatorType":"EQUALS","values":[1]}`, true},
			validateCase{where + ": group", s, `{"type":"group","operator":"AND","conditions":[]}`, true},
			validateCase{where + ": function", s, `{"type":"function","function":{"name":"f"}}`, true},
			validateCase{where + ": unknown type", s, `{"type":"bogus"}`, false},
			validateCase{where + ": no type", s, `{"jsonPath":"$.a","operatorType":"EQUALS","value":1}`, false},
			validateCase{where + ": simple without jsonPath", s, `{"type":"simple","operatorType":"EQUALS","value":1}`, false},
			validateCase{where + ": simple with a lifecycle's fields", s, `{"type":"simple","field":"state","operatorType":"EQUALS"}`, false},
			validateCase{where + ": group without operator", s, `{"type":"group","conditions":[]}`, false},
			validateCase{where + ": function without function", s, `{"type":"function"}`, false},
		)
	}
	validateCases(t, doc, cases)
}

// Exported workflow criteria validate as the server writes them, and a
// shape the server refuses for its form does not validate. Where `function`
// is valid is the server's rule, not the schema's: a `function` condition in
// a search body or a pre-check validates, and the server refuses it (400
// INVALID_CONDITION and 400 VALIDATION_FAILED).
func TestCriterionShapesValidate(t *testing.T) {
	doc := viewedSpec(t)
	transition := doc.Components.Schemas["TransitionDefinitionDto"].Value
	group := doc.Components.Schemas["GroupConditionDto"].Value
	simple := doc.Components.Schemas["SimpleConditionDto"].Value
	lifecycle := doc.Components.Schemas["LifecycleConditionDto"].Value
	tr := func(criterion string) string {
		return `{"name":"p","next":"X","manual":false,"criterion":` + criterion + `}`
	}
	validateCases(t, doc, []validateCase{
		{"group with a function member and no pre-check", transition, tr(`{"type":"group","operator":"AND","conditions":[
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10},
			{"type":"group","operator":"NOT","conditions":[{"type":"function","function":{"name":"a","criterion":
				{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}}}]},
			{"type":"function","function":{"name":"b"}}]}`), true},
		{"function with a null pre-check", transition, tr(`{"type":"function","function":{"name":"f","criterion":null}}`), true},
		{"function with a group pre-check", transition, tr(`{"type":"function","function":{"name":"f","criterion":
			{"type":"group","operator":"OR","conditions":[{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":["a"]}]}}}`), true},
		{"simple criterion with a scalar value", transition, tr(`{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1}`), true},
		{"function in a pre-check (refused by import)", transition, tr(`{"type":"function","function":{"name":"f","criterion":{"type":"function","function":{"name":"g"}}}}`), true},
		{"unknown condition type", transition, tr(`{"type":"bogus"}`), false},
		{"simple without jsonPath", transition, tr(`{"type":"simple","operatorType":"EQUALS","value":1}`), false},
		{"unknown type in a pre-check", transition, tr(`{"type":"function","function":{"name":"f","criterion":{"type":"bogus"}}}`), false},
		{"function member in a group (refused in search)", group, `{"type":"group","operator":"AND","conditions":[{"type":"function","function":{"name":"f"}}]}`, true},
		{"unknown member in a group", group, `{"type":"group","operator":"AND","conditions":[{"type":"bogus"}]}`, false},
		{"group with an unknown operator", group, `{"type":"group","operator":"XOR","conditions":[]}`, false},
		// kin accepts any value where a schema recurs; ValidateValue's walk
		// checks it.
		{"null in a group", group, `{"type":"group","operator":"AND","conditions":[null]}`, false},
		{"a number in a group", group, `{"type":"group","operator":"AND","conditions":[42]}`, false},
		{"a string in a group", group, `{"type":"group","operator":"AND","conditions":["x"]}`, false},
		{"null in a group in a group", group, `{"type":"group","operator":"AND","conditions":[
			{"type":"group","operator":"OR","conditions":[null]}]}`, false},
		{"null in a group criterion", transition, tr(`{"type":"group","operator":"AND","conditions":[null, 42, "x"]}`), false},
		{"unknown type in a pre-check's group", transition, tr(`{"type":"function","function":{"name":"f","criterion":
			{"type":"group","operator":"AND","conditions":[{"type":"group","operator":"AND","conditions":[{"type":"bogus"}]}]}}}`), false},
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
	})
}
