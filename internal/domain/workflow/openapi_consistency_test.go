package workflow

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIWorkflowVersionContract asserts that
// WorkflowConfigurationDto.version in api/openapi.yaml carries the
// exact pattern enforced by the Go ParseSchemaVersion parser and
// names the help-topic discovery endpoint in its description. Drift
// between YAML and Go becomes a test failure here, not a runtime
// surprise.
func TestOpenAPIWorkflowVersionContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	comps, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("components missing or wrong shape")
	}
	schemas, ok := comps["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("components.schemas missing or wrong shape")
	}
	wcd, ok := schemas["WorkflowConfigurationDto"].(map[string]any)
	if !ok {
		t.Fatalf("WorkflowConfigurationDto missing")
	}
	props, ok := wcd["properties"].(map[string]any)
	if !ok {
		t.Fatalf("WorkflowConfigurationDto.properties missing")
	}
	ver, ok := props["version"].(map[string]any)
	if !ok {
		t.Fatalf("WorkflowConfigurationDto.version missing")
	}
	pattern, _ := ver["pattern"].(string)
	const wantPattern = `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
	if pattern != wantPattern {
		t.Fatalf("version.pattern = %q; want %q", pattern, wantPattern)
	}
	desc, _ := ver["description"].(string)
	for _, want := range []string{"MAJOR.MINOR", "/help/workflows/schema-version/versions"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("version.description does not contain %q. Got:\n%s", want, desc)
		}
	}
}
