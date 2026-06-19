package workflow

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestDefaultWorkflowFixtureSchemaVersion is a drift guard. The
// embedded default_workflow.json bypasses the import-time
// validateSchemaVersions check, so a stale Version here would not
// be caught at runtime. This test ensures the fixture's declared
// version always matches CurrentSchemaVersion.
func TestDefaultWorkflowFixtureSchemaVersion(t *testing.T) {
	t.Parallel()
	var wf spi.WorkflowDefinition
	if err := json.Unmarshal(defaultWorkflowJSON, &wf); err != nil {
		t.Fatalf("unmarshal default_workflow.json: %v", err)
	}
	if wf.Version != CurrentSchemaVersion {
		t.Fatalf("default_workflow.json version = %q; want %q (= CurrentSchemaVersion)", wf.Version, CurrentSchemaVersion)
	}
}
