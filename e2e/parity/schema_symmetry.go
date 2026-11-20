package parity

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// RunDeepSchemaSymmetry exercises deeply nested objects (4 levels),
// arrays with nulls and empty strings, JSON numbers at the int/float
// boundary, large strings (2KB), and unicode with combining characters.
// Asserts semantic equality on round-trip through entity create + get.
func RunDeepSchemaSymmetry(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "deep-schema-test"
	const modelVersion = 1

	// Import a model whose sample matches the complex structure we will
	// create, so field-level validation passes.
	setupSchemaSymmetryWorkflow(t, c, modelName, modelVersion)

	// Build a complex data structure that exercises edge cases
	// across storage backends.
	complex := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"level4_int":     float64(42),
					"level4_float":   3.14159,
					"level4_string":  "hello",
					"level4_unicode": "h\u00e9llo w\u00f6rld \U0001F30D",
					"level4_empty":   "",
					"level4_null":    nil,
				},
			},
		},
		"array_with_nulls": []any{"a", nil, "", "b"},
		"large_string":     strings.Repeat("x", 2048),
		"int_at_boundary":  float64(1<<53 - 1), // max safe integer in float64
		"negative_zero":    float64(0),         // -0 becomes 0 in JSON
		"boolean_true":     true,
		"boolean_false":    false,
		"empty_object":     map[string]any{},
		"empty_array":      []any{},
	}

	body, err := json.Marshal(complex)
	if err != nil {
		t.Fatalf("marshal complex data: %v", err)
	}

	entityID, err := c.CreateEntity(t, modelName, modelVersion, string(body))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	// Normalize both sides through json.Marshal/Unmarshal so type
	// representations match (e.g., both sides produce float64 for
	// numbers, both represent nil as nil, etc.).
	want := normalizeJSON(t, complex)
	actual := normalizeJSON(t, got.Data)

	if !reflect.DeepEqual(actual, want) {
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		actualJSON, _ := json.MarshalIndent(actual, "", "  ")
		t.Errorf("round-trip data mismatch:\n--- want ---\n%s\n--- got ---\n%s",
			string(wantJSON), string(actualJSON))
	}
}

// setupSchemaSymmetryWorkflow imports a model whose sample document
// matches the complex nested structure used by RunDeepSchemaSymmetry,
// locks it, and imports the simple auto-transition workflow.
func setupSchemaSymmetryWorkflow(t *testing.T, c *client.Client, modelName string, modelVersion int) {
	t.Helper()

	// The sample document must contain every top-level and nested field
	// that the test entity will use AND seed each leaf's classification
	// to the type the test will later POST. Strict validation under the
	// A.1 IsAssignableTo semantics rejects LONG against an INTEGER schema
	// and DOUBLE against an INTEGER schema, so the sample must pre-seed
	// those leaves with the broader type.
	sampleDoc := `{
		"level1": {"level2": {"level3": {"level4_int": 0, "level4_float": 0.5, "level4_string": "", "level4_unicode": "", "level4_empty": "", "level4_null": null}}},
		"array_with_nulls": ["sample", null],
		"large_string": "",
		"int_at_boundary": 9007199254740991,
		"negative_zero": 0,
		"boolean_true": true,
		"boolean_false": false,
		"empty_object": {},
		"empty_array": []
	}`

	if err := c.ImportModel(t, modelName, modelVersion, sampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, simpleWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// normalizeJSON re-marshals and re-unmarshals v through encoding/json
// so both sides of a DeepEqual comparison have identical Go types
// (float64 for all numbers, nil for JSON null, etc.).
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normalizeJSON marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("normalizeJSON unmarshal: %v", err)
	}
	return out
}
