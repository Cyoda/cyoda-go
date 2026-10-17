package openapivalidator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// validateFixtureValue checks a JSON value against the named component
// schema of the recursive fixture, through the view.
func validateFixtureValue(t *testing.T, schema, value string, opts ...openapi3.SchemaValidationOption) error {
	t.Helper()
	doc := loadSpec(t, recursiveFixtureYAML)
	if err := ApplyView(doc); err != nil {
		t.Fatalf("ApplyView: %v", err)
	}
	var v any
	if err := json.Unmarshal([]byte(value), &v); err != nil {
		t.Fatalf("decode %s: %v", value, err)
	}
	return ValidateValue(doc, doc.Components.Schemas[schema].Value, v, opts...)
}

// kin accepts any value where Group recurs, so these pass a plain VisitJSON;
// ValidateValue reports each of them.
func TestValidateValue_ReportsValuesWhereASchemaRecurs(t *testing.T) {
	for name, c := range map[string]struct {
		value string
		want  string
	}{
		"null in a group":            {`{"type":"group","conditions":[null]}`, `recursive schema at "/conditions/0": Value is not nullable`},
		"a number in a group":        {`{"type":"group","conditions":[42]}`, `recursive schema at "/conditions/0": `},
		"a string in a group":        {`{"type":"group","conditions":["x"]}`, `recursive schema at "/conditions/0": `},
		"null in a group in a group": {`{"type":"group","conditions":[{"type":"group","conditions":[null]}]}`, `recursive schema at "/conditions/0/conditions/0": Value is not nullable`},
		// The fresh check of the group at /conditions/0 reports its own
		// /conditions/0, the simple condition, with a path relative to it.
		"a defect two levels down": {`{"type":"group","conditions":[{"type":"group","conditions":[
			{"type":"simple","value":"not-an-integer"}]}]}`, `recursive schema at "/conditions/0": Error at "/conditions/0": doesn't match schema due to: Error at "/value": value must be an integer`},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateFixtureValue(t, "Group", c.value)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not contain %s: %v", c.want, err)
			}
		})
	}
}

// Every defect is listed, kin's own and the walk's, each once.
func TestValidateValue_ListsEveryDefectOnce(t *testing.T) {
	err := validateFixtureValue(t, "Group", `{"type":"group","conditions":[
		{"type":"simple","value":"a"},
		{"type":"group","conditions":[null, {"type":"simple","value":"b"}]}]}`)
	var multi openapi3.MultiError
	if !errors.As(err, &multi) {
		t.Fatalf("want a MultiError, got %T: %v", err, err)
	}
	if len(multi) != 3 {
		t.Fatalf("want 3 defects (kin's /conditions/0 and the walk's two), got %d: %v", len(multi), multi)
	}
}

// kin refuses a null no union member accepts once per member, with the same
// pointer and reason; ValidateValue lists it once.
func TestValidateValue_KinRowsListedOnce(t *testing.T) {
	err := validateFixtureValue(t, "Condition", `null`)
	var multi openapi3.MultiError
	if !errors.As(err, &multi) {
		t.Fatalf("want a MultiError, got %T: %v", err, err)
	}
	if len(multi) != 1 {
		t.Fatalf("want 1 row, got %d: %v", len(multi), multi)
	}
}

func TestValidateValue_ValidNestedValuePasses(t *testing.T) {
	value := `{"type":"group","conditions":[
		{"type":"simple","value":1},
		{"type":"group","conditions":[
			{"type":"group","conditions":[{"type":"simple","value":2},{"type":"group","conditions":[]}]}]}]}`
	if err := validateFixtureValue(t, "Group", value); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if err := validateFixtureValue(t, "Condition", value); err != nil {
		t.Fatalf("refused as a Condition: %v", err)
	}
}

// opts replace the default options: here kin's own visitor alone.
func TestValidateValue_OptionsReplaceTheDefault(t *testing.T) {
	if err := validateFixtureValue(t, "Group", `{"type":"group","conditions":[null]}`, openapi3.MultiErrors()); err == nil {
		t.Fatal("accepted with explicit options")
	}
	if err := validateFixtureValue(t, "Group", `{"type":"group","conditions":[]}`, openapi3.MultiErrors()); err != nil {
		t.Fatalf("refused with explicit options: %v", err)
	}
}
