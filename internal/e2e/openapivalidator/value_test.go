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

// A defect in a value below a recursion of Group is reported, with a chain of
// pointers, each relative to the one before.
func TestValidateValue_ReportsValuesWhereASchemaRecurs(t *testing.T) {
	for name, c := range map[string]struct {
		value string
		want  string
	}{
		"null in a group":            {`{"type":"group","conditions":[null]}`, `Error at "/conditions/0": doesn't match schema due to: Value is not nullable`},
		"a number in a group":        {`{"type":"group","conditions":[42]}`, `Error at "/conditions/0": doesn't match schema due to: value must be an object`},
		"a string in a group":        {`{"type":"group","conditions":["x"]}`, `Error at "/conditions/0": doesn't match schema due to: value must be an object`},
		"null in a group in a group": {`{"type":"group","conditions":[{"type":"group","conditions":[null]}]}`, `Error at "/conditions/0": doesn't match schema due to: Error at "/conditions/0": doesn't match schema due to: Value is not nullable`},
		"a defect two levels down": {`{"type":"group","conditions":[{"type":"group","conditions":[
			{"type":"simple","value":"not-an-integer"}]}]}`, `Error at "/conditions/0": doesn't match schema due to: Error at "/conditions/0": doesn't match schema due to: Error at "/value": value must be an integer`},
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

// Every defect is listed: one row per top-level condition, and the two
// defects of the nested group inside its row.
func TestValidateValue_ListsEveryDefect(t *testing.T) {
	err := validateFixtureValue(t, "Group", `{"type":"group","conditions":[
		{"type":"simple","value":"a"},
		{"type":"group","conditions":[null, {"type":"simple","value":"b"}]}]}`)
	var multi openapi3.MultiError
	if !errors.As(err, &multi) {
		t.Fatalf("want a MultiError, got %T: %v", err, err)
	}
	if len(multi) != 2 {
		t.Fatalf("want 2 rows (/conditions/0 and /conditions/1), got %d: %v", len(multi), multi)
	}
	for i, want := range []string{
		`Error at "/conditions/0": doesn't match schema due to: Error at "/value": value must be an integer`,
		`Error at "/conditions/1": doesn't match schema due to: Error at "/conditions/0": doesn't match schema due to: Value is not nullable`,
	} {
		if !strings.Contains(multi[i].Error(), want) {
			t.Errorf("row %d = %v\nwant it to contain %s", i, multi[i], want)
		}
	}
	if want := `Error at "/conditions/1": doesn't match schema due to: Error at "/value": value must be an integer`; !strings.Contains(multi[1].Error(), want) {
		t.Errorf("row 1 = %v\nwant it to contain %s", multi[1], want)
	}
}

// kin refuses a null no union member accepts with one error that holds each
// member's; ValidateValue splits only the top level, so it lists one row.
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
